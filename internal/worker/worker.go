package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/configservice"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/configctl/internal/config"
	"github.com/configctl/internal/report"
)

const serviceLinkedPrefix = "AWSConfigurationRecorderFor"

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
			Region: item.Region,
			Error:  &errMsg,
		})
		return
	}

	cfgClient := configservice.NewFromConfig(regionalCfg)

	// Describe ALL recorders (customer-managed + service-linked).
	recorders, err := cfgClient.DescribeConfigurationRecorders(ctx, &configservice.DescribeConfigurationRecordersInput{})
	if err != nil {
		errMsg := fmt.Sprintf("describing recorders: %v", err)
		logger.Error("failed to describe recorders", "error", err)
		p.collector.Add(item.AccountID, report.RegionReport{
			Region: item.Region,
			Error:  &errMsg,
		})
		return
	}

	// Get status for all recorders.
	var allNames []string
	for _, rec := range recorders.ConfigurationRecorders {
		allNames = append(allNames, aws.ToString(rec.Name))
	}

	statusMap := make(map[string]bool) // name -> recording
	if len(allNames) > 0 {
		statusOut, err := cfgClient.DescribeConfigurationRecorderStatus(ctx, &configservice.DescribeConfigurationRecorderStatusInput{
			ConfigurationRecorderNames: allNames,
		})
		if err != nil {
			// Non-fatal: we can still report recorders exist, just without status.
			logger.Warn("failed to describe recorder statuses", "error", err)
		} else {
			for _, s := range statusOut.ConfigurationRecordersStatus {
				statusMap[aws.ToString(s.Name)] = s.Recording
			}
		}
	}

	// Classify recorders.
	regionReport := report.RegionReport{
		Region: item.Region,
	}

	for _, rec := range recorders.ConfigurationRecorders {
		name := aws.ToString(rec.Name)
		recording := statusMap[name]

		if isServiceLinked(name) {
			// Service-linked recorder — report only, never stop.
			slr := report.RecorderReport{
				Name:          name,
				WasEnabled:    recording,
				CurrentStatus: boolToStatus(recording),
			}

			// Extract service principal if available.
			if rec.ServicePrincipal != nil {
				slr.ServicePrincipal = aws.ToString(rec.ServicePrincipal)
			}

			// Extract recording scope if available.
			if rec.RecordingScope != "" {
				slr.RecordingScope = string(rec.RecordingScope)
			}

			logger.Info("found service-linked recorder",
				"recorder", name,
				"recording", recording,
				"servicePrincipal", slr.ServicePrincipal,
				"recordingScope", slr.RecordingScope,
			)

			regionReport.ServiceLinkedRecorders = append(regionReport.ServiceLinkedRecorders, slr)
			continue
		}

		// Customer-managed recorder.
		cr := p.handleCustomerRecorder(ctx, logger, cfgClient, name, recording)
		regionReport.CustomerRecorder = &cr
	}

	// If no customer-managed recorder was found, mark absent.
	if regionReport.CustomerRecorder == nil {
		logger.Info("no customer-managed recorder found")
		regionReport.CustomerRecorder = nil // explicit nil for absent
	}

	p.collector.Add(item.AccountID, regionReport)
}

// handleCustomerRecorder processes the customer-managed "default" recorder.
// In dry-run it reports state; in live mode it stops the recorder if active.
func (p *Pool) handleCustomerRecorder(
	ctx context.Context,
	logger *slog.Logger,
	client *configservice.Client,
	name string,
	recording bool,
) report.RecorderReport {
	if p.dryRun {
		logger.Info("dry-run: checked customer recorder", "recorder", name, "recording", recording)
		return report.RecorderReport{
			Name:          name,
			WasEnabled:    recording,
			CurrentStatus: boolToStatus(recording),
		}
	}

	// Live run.
	if !recording {
		logger.Info("customer recorder already stopped", "recorder", name)
		return report.RecorderReport{
			Name:          name,
			WasEnabled:    false,
			CurrentStatus: "disabled",
		}
	}

	_, err := client.StopConfigurationRecorder(ctx, &configservice.StopConfigurationRecorderInput{
		ConfigurationRecorderName: aws.String(name),
	})
	if err != nil {
		errMsg := fmt.Sprintf("stopping recorder: %v", err)
		logger.Error("failed to stop customer recorder", "recorder", name, "error", err)
		return report.RecorderReport{
			Name:          name,
			WasEnabled:    true,
			CurrentStatus: "error",
			Error:         &errMsg,
		}
	}

	logger.Info("stopped customer recorder", "recorder", name)
	return report.RecorderReport{
		Name:          name,
		WasEnabled:    true,
		CurrentStatus: "stopped",
	}
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

// isServiceLinked returns true if the recorder name indicates a service-linked recorder.
func isServiceLinked(name string) bool {
	return strings.HasPrefix(name, serviceLinkedPrefix)
}

func boolToStatus(recording bool) string {
	if recording {
		return "enabled"
	}
	return "disabled"
}
