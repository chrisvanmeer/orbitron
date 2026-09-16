package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"orbitron/internal/auth"
	"orbitron/internal/config"
	"orbitron/internal/installer"
	"orbitron/internal/logger"
	"orbitron/internal/logo"
	"orbitron/internal/pruner"
	"orbitron/internal/server"
)

func main() {
	configPath := flag.String("config", "/etc/orbitron/config.yml", "Path to configuration file")
	doInstall := flag.Bool("install", false, "Install Orbitron service, user, and logrotate")
	doUninstall := flag.Bool("uninstall", false, "Uninstall Orbitron service, user, and data")
	genToken := flag.Bool("generate-token", false, "Generate an administrative Bearer token")
	revokeToken := flag.String("revoke-token", "", "Revoke a Bearer token")
	doPrune := flag.Bool("prune", false, "Clean up unreferenced roles and collections")

	quiet := flag.Bool("quiet", false, "Output raw token only")
	flag.BoolVar(quiet, "q", false, "Output raw token only (shorthand)")

	flag.Parse()

	if *doInstall {
		if err := installer.RunInstall(); err != nil {
			logger.Error("Installation failed: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if *doUninstall {
		if err := installer.RunUninstall(); err != nil {
			logger.Error("Uninstallation failed: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if *genToken {
		cfg, err := config.LoadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
			os.Exit(1)
		}

		token, err := auth.GenerateToken(cfg.TokensFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to generate token: %v\n", err)
			os.Exit(1)
		}

		if *quiet {
			fmt.Print(token)
		} else {
			fmt.Printf("Generated new administrative Bearer Token: %s\n", token)
		}
		os.Exit(0)
	}

	if *revokeToken != "" {
		cfg, err := config.LoadConfig(*configPath)
		if err != nil {
			logger.Error("Failed to load config: %v", err)
			os.Exit(1)
		}

		if err := auth.RevokeToken(cfg.TokensFile, *revokeToken); err != nil {
			logger.Error("Failed to revoke token: %v", err)
			os.Exit(1)
		}
		logger.Info("Token revoked successfully")
		os.Exit(0)
	}

	if *doPrune {
		cfg, err := config.LoadConfig(*configPath)
		if err != nil {
			logger.Error("Failed to load config: %v", err)
			os.Exit(1)
		}

		p := pruner.NewPruner(cfg.StoragePath)
		if err := p.RunPrune(); err != nil {
			logger.Error("Pruning failed: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	logo.PrintBanner()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		logger.Error("Failed to load config from %s: %v", *configPath, err)
		os.Exit(1)
	}

	if err := logger.Init(cfg.LogPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize log file %s: %v\n", cfg.LogPath, err)
	}
	defer logger.Close()

	srv := server.NewServer(cfg)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(); err != nil {
			logger.Error("Server error: %v", err)
			os.Exit(1)
		}
	}()

	<-stop
	logger.Info("Shutting down Orbitron daemon...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("Error shutting down server: %v", err)
	}
	logger.Info("Orbitron stopped gracefully")
}
