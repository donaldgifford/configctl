package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/configservice"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/donaldgifford/configctl/internal/config"
	"github.com/donaldgifford/configctl/internal/report"
)

type Pool struct {
	cfg       *config.Config
	baseCfg   aws.Config
	collector *report.Collector
	dryRun    bool
	logger    *slog.Logger
}

func NewPool(cfg *config.Config, dryRun bool, logger *slog.Logger) (*Pool, error) {
	backoff, _ := cfg.Concurrency.BackoffDuration()

	if cfg.AWSProfile == "" {
		return nil, fmt.Errorf("awsProfile must be set in config or via --aws-profile flag")
	}

	logger.Info("using AWS profile", "profile", cfg.AWSProfile)

	baseCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithSharedConfigProfile(cfg.AWSProfile),
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = cfg.Concurrency.RetryMaxAttempts
				o.MaxBackoff = backoff
			})
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Pool{
		cfg:       cfg,
		baseCfg:   baseCfg,
		collector: report.NewCollector(),
		dryRun:    dryRun,
		logger:    logger,
	}, nil
}

// DetectManagementAccount calls Organizations to find the management account
// and marks it in the collector for the report.
func (p *Pool) DetectManagementAccount(ctx context.Context) (string, error) {
	orgClient := organizations.NewFromConfig(p.baseCfg)

	out, err := orgClient.DescribeOrganization(ctx, &organizations.DescribeOrganizationInput{})
	if err != nil {
		return "", fmt.Errorf("describing organization: %w", err)
	}

	mgmtAcct := aws.ToString(out.Organization.MasterAccountId)
	p.collector.SetManagementAccount(mgmtAcct)
	p.logger.Info("detected management account", "accountId", mgmtAcct)

	return mgmtAcct, nil
}

// Run fans out work items across the worker pool and returns the built report.
func (p *Pool) Run(ctx context.Context) *report.Report {
	items := p.cfg.WorkItems()

	ch := make(chan config.WorkItem, len(items))
	for _, item := range items {
		ch <- item
	}
	close(ch)

	var wg sync.WaitGroup
	for i := 0; i < p.cfg.Concurrency.MaxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range ch {
				p.process(ctx, item)
			}
		}()
	}

	wg.Wait()

	return p.collector.Build(p.dryRun, p.cfg.Accounts)
}

func (p *Pool) process(ctx context.Context, item config.WorkItem) {
	logger := p.logger.With("account", item.AccountID, "region", item.Region)

	// Build regional config with assumed role credentials.
	regionalCfg, err := p.assumeRegionalConfig(ctx, item)
	if err != nil {
		errMsg := err.Error()
		logger.Error("failed to assume role", "error", err)
		p.collector.Add(item.AccountID, report.RegionReport{
			Region:        item.Region,
			CurrentStatus: "error",
			Error:         &errMsg,
		})
		return
	}

	cfgClient := configservice.NewFromConfig(regionalCfg)

	// Describe existing recorders.
	recorders, err := cfgClient.DescribeConfigurationRecorders(
		ctx,
		&configservice.DescribeConfigurationRecordersInput{},
	)
	if err != nil {
		errMsg := fmt.Sprintf("describing recorders: %v", err)
		logger.Error("failed to describe recorders", "error", err)
		p.collector.Add(item.AccountID, report.RegionReport{
			Region:        item.Region,
			CurrentStatus: "error",
			Error:         &errMsg,
		})
		return
	}

	if len(recorders.ConfigurationRecorders) == 0 {
		logger.Info("no recorder found")
		p.collector.Add(item.AccountID, report.RegionReport{
			Region:        item.Region,
			CurrentStatus: "absent",
		})
		return
	}

	recorderName := aws.ToString(recorders.ConfigurationRecorders[0].Name)

	// Check recorder status.
	statusOut, err := cfgClient.DescribeConfigurationRecorderStatus(
		ctx,
		&configservice.DescribeConfigurationRecorderStatusInput{
			ConfigurationRecorderNames: []string{recorderName},
		},
	)
	if err != nil {
		errMsg := fmt.Sprintf("describing recorder status: %v", err)
		logger.Error("failed to describe recorder status", "error", err)
		p.collector.Add(item.AccountID, report.RegionReport{
			Region:        item.Region,
			RecorderName:  recorderName,
			CurrentStatus: "error",
			Error:         &errMsg,
		})
		return
	}

	wasRecording := false
	if len(statusOut.ConfigurationRecordersStatus) > 0 {
		wasRecording = statusOut.ConfigurationRecordersStatus[0].Recording
	}

	if p.dryRun {
		status := "disabled"
		if wasRecording {
			status = "enabled"
		}
		logger.Info("dry-run: checked recorder", "recorder", recorderName, "recording", wasRecording)
		p.collector.Add(item.AccountID, report.RegionReport{
			Region:        item.Region,
			RecorderName:  recorderName,
			WasEnabled:    wasRecording,
			CurrentStatus: status,
		})
		return
	}

	// Live run: stop the recorder if it's recording.
	if !wasRecording {
		logger.Info("recorder already stopped", "recorder", recorderName)
		p.collector.Add(item.AccountID, report.RegionReport{
			Region:        item.Region,
			RecorderName:  recorderName,
			WasEnabled:    false,
			CurrentStatus: "disabled",
		})
		return
	}

	_, err = cfgClient.StopConfigurationRecorder(ctx, &configservice.StopConfigurationRecorderInput{
		ConfigurationRecorderName: aws.String(recorderName),
	})
	if err != nil {
		errMsg := fmt.Sprintf("stopping recorder: %v", err)
		logger.Error("failed to stop recorder", "error", err)
		p.collector.Add(item.AccountID, report.RegionReport{
			Region:        item.Region,
			RecorderName:  recorderName,
			WasEnabled:    true,
			CurrentStatus: "error",
			Error:         &errMsg,
		})
		return
	}

	logger.Info("stopped recorder", "recorder", recorderName)
	p.collector.Add(item.AccountID, report.RegionReport{
		Region:        item.Region,
		RecorderName:  recorderName,
		WasEnabled:    true,
		CurrentStatus: "stopped",
	})
}

func (p *Pool) assumeRegionalConfig(ctx context.Context, item config.WorkItem) (aws.Config, error) {
	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", item.AccountID, p.cfg.AssumeRole)

	stsClient := sts.NewFromConfig(p.baseCfg)
	creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = fmt.Sprintf("configctl-%s-%d", item.AccountID, time.Now().Unix())
	})

	backoff, _ := p.cfg.Concurrency.BackoffDuration()

	regionalCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(item.Region),
		awsconfig.WithCredentialsProvider(aws.NewCredentialsCache(creds)),
		awsconfig.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = p.cfg.Concurrency.RetryMaxAttempts
				o.MaxBackoff = backoff
			})
		}),
	)
	if err != nil {
		return aws.Config{}, fmt.Errorf("loading regional config for %s/%s: %w", item.AccountID, item.Region, err)
	}

	return regionalCfg, nil
}
