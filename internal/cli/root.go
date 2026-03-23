package cli

import (
	"github.com/spf13/cobra"

	"github.com/nitinmore/datamigrate/internal/util"
)

var (
	logLevel  string
	logJSON   bool
)

var rootCmd = &cobra.Command{
	Use:   "datamigrate",
	Short: "VMware to Nutanix AHV VM migration tool",
	Long:  "datamigrate migrates VMs from VMware vSphere to Nutanix AHV using block-level replication with near-zero downtime.",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		util.SetupLogging(logLevel, logJSON)
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().BoolVar(&logJSON, "log-json", false, "Output logs as JSON")
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
