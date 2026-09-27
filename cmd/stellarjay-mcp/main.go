// Command stellarjay-mcp serves the AvianSuite MCP tools over stdio for one
// Stellar Jay store, for MCP clients that launch a local command.
//
//	STELLARJAY_URL=https://store.example.com STELLARJAY_TOKEN=... stellarjay-mcp
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kyle-visner/stellarjay"
	"github.com/kyle-visner/stellarjay/client"
	"github.com/kyle-visner/stellarjay/mcp"
)

func main() {
	// stdout carries the protocol, so logs go to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stellarjay-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Println("usage: STELLARJAY_URL=https://... STELLARJAY_TOKEN=... stellarjay-mcp")
			fmt.Println("Serves the AvianSuite MCP tools over stdio. Set STELLARJAY_INSECURE_HTTP=1 to allow a non-loopback http:// URL.")
			return nil
		case "version", "--version":
			fmt.Println(mcp.Version)
			return nil
		}
		return fmt.Errorf("unknown argument %q; run with --help", os.Args[1])
	}
	baseURL := stellarjay.Getenv("STELLARJAY_URL")
	if baseURL == "" {
		return fmt.Errorf("STELLARJAY_URL is required")
	}
	c, err := client.New(baseURL, stellarjay.Getenv("STELLARJAY_TOKEN"), client.Options{
		AllowInsecureHTTP: stellarjay.Getenv("STELLARJAY_INSECURE_HTTP") == "1",
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcp.NewServer().ServeStdio(ctx, c, os.Stdin, os.Stdout)
}
