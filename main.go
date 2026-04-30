package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.1.0"

// rootCmd is the base command for engram CLI
var rootCmd = &cobra.Command{
	Use:   "engram",
	Short: "engram — a persistent memory layer for AI assistants",
	Long: `engram is a lightweight, file-based memory system that enables
AI assistants to store, retrieve, and reason over long-term context
across sessions using chunked JSONL storage.`,
	SilenceUsage: true,
}

// versionCmd prints the current version
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version of engram",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("engram v%s\n", version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
