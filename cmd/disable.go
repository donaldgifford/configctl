package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/configctl/internal/config"
	"github.com/configctl/internal/report"
	"github.com/configctl/internal/worker"
)

var (
	configFile string
	dryRun     bool
	outputFile string
	awsProfile string
)

var disableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Stop AWS Config recorders across accounts and regions",
	Long: `Stops AWS Config configuration recorders across all specified accounts and
regions. In dry-run mode, reports the current state without making changes.

Requires an AWS SSO session for the management account. Login first:
  aws sso login --profile <mgmt-profile>

Example:
  configctl disable --config config.yaml --aws-profile mgmt-prod --dry-run --output report.json
  configctl disable --config config.yaml --aws-profile mgmt-prod --output report.json`,
	RunE: runDisable,
}

func init() {
	disableCmd.Flags().StringVarP(&configFile, "config", "c", "config.yaml", "path to YAML configuration file")
	disableCmd.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "check recorder status without making changes")
	disableCmd.Flags().StringVarP(&outputFile, "output", "o", "report.json", "path to write JSON report")
	disableCmd.Flags().StringVarP(&awsProfile, "aws-profile", "p", "", "AWS CLI profile (overrides config file awsProfile)")

	rootCmd.AddCommand(disableCmd)
}

func runDisable(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if dryRun {
		logger.Info("running in dry-run mode — no changes will be made")
	} else {
		logger.Warn("running in LIVE mode — recorders will be stopped")
	}

	// Load and validate config.
	cfg, err := config.Load(configFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// CLI flag overrides config file value.
	if awsProfile != "" {
		cfg.AWSProfile = awsProfile
	}
	if cfg.AWSProfile == "" {
		return fmt.Errorf("AWS profile must be set via --aws-profile flag or awsProfile in config")
	}

	totalItems := len(cfg.Accounts) * len(cfg.Regions)
	logger.Info("loaded configuration",
		"accounts", len(cfg.Accounts),
		"regions", len(cfg.Regions),
		"workItems", totalItems,
		"maxWorkers", cfg.Concurrency.MaxWorkers,
	)

	// Initialize worker pool.
	pool, err := worker.NewPool(cfg, dryRun, logger)
	if err != nil {
		return fmt.Errorf("initializing worker pool: %w", err)
	}

	// Detect management account for the report.
	mgmtAcct, err := pool.DetectManagementAccount(ctx)
	if err != nil {
		logger.Warn("could not detect management account — continuing without management account tagging", "error", err)
	} else {
		// Check if management account is in our target list.
		for _, acct := range cfg.Accounts {
			if acct == mgmtAcct {
				logger.Warn("management account is in target list — SCP does not protect this account", "accountId", mgmtAcct)
				break
			}
		}
	}

	// Run the work.
	result := pool.Run(ctx)

	// Print summary to stderr.
	logger.Info("run complete",
		"customerRecordersEnabled", result.Summary.CustomerRecordersEnabled,
		"customerRecordersDisabled", result.Summary.CustomerRecordersDisabled,
		"customerRecordersAbsent", result.Summary.CustomerRecordersAbsent,
		"serviceLinkedRecorders", result.Summary.ServiceLinkedRecorders,
		"serviceLinkedRecordersPaid", result.Summary.ServiceLinkedRecordersPaid,
		"errors", result.Summary.Errors,
	)

	if result.Summary.ServiceLinkedRecorders > 0 {
		logger.Warn("service-linked recorders detected — these cannot be stopped via Config API, disable them through the owning AWS service",
			"total", result.Summary.ServiceLinkedRecorders,
			"paid", result.Summary.ServiceLinkedRecordersPaid,
		)
	}

	// Write report.
	if err := report.WriteJSON(result, outputFile); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}

	logger.Info("report written", "path", outputFile)

	if result.Summary.Errors > 0 {
		return fmt.Errorf("%d errors encountered — review report for details", result.Summary.Errors)
	}

	return nil
}
