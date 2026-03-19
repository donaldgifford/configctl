package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "configctl",
	Short: "Manage AWS Config recorders across an organization",
	Long: `configctl is a CLI tool for managing AWS Config configuration recorders
across multiple AWS accounts and regions. It supports dry-run checks and
live operations to stop recorders, with configurable concurrency and
automatic retry with exponential backoff.`,
}

func Execute() error {
	return rootCmd.Execute()
}
