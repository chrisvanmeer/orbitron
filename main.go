package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"orbitron/auth"
	"orbitron/config"
	"orbitron/installer"
	"orbitron/logger"
	"orbitron/logo"
	"orbitron/pruner"
	"orbitron/server"
)

func main() {
	logo.PrintBanner()

	installFlag := flag.Bool("install", false, "Install Orbitron to systemd and create system paths")
	uninstallFlag := flag.Bool("uninstall", false, "Uninstall Orbitron and remove system configurations")
	pruneFlag := flag.Bool("prune", false, "Remove older/unreferenced role and collection versions")
	genTokenFlag := flag.Bool("generate-token", false, "Generate a new authentication token")
	revokeTokenFlag := flag.String("revoke-token", "", "Revoke a specific authentication token")
	configFlag := flag.String("config", "/etc/orbitron/config.yml", "Path to config file")

	flag.Parse()

	if *installFlag {
		if err := installer.RunInstall(); err != nil {
			fmt.Printf("Error during installation: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *uninstallFlag {
		if err := installer.RunUninstall(); err != nil {
			fmt.Printf("Error during uninstallation: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *pruneFlag {
		cfg, err := config.LoadConfig(*configFlag)
		if err != nil {
			fmt.Printf("Error loading configuration: %v\n", err)
			os.Exit(1)
		}
		p := pruner.NewPruner(cfg.StoragePath)
		if err := p.RunPrune(); err != nil {
			fmt.Printf("Error during prune operation: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *genTokenFlag {
		token, err := auth.IssueToken(auth.TokensFilePath)
		if err != nil {
			fmt.Printf("Error generating token: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("New Auth Token generated successfully:\n\n  %s\n\nSave this token in your Ansible Vault.\n", token)
		return
	}

	if *revokeTokenFlag != "" {
		if err := auth.RevokeToken(auth.TokensFilePath, *revokeTokenFlag); err != nil {
			fmt.Printf("Error revoking token: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Token revoked successfully.")
		return
	}

	// Daemon Mode
	cfg, err := config.LoadConfig(*configFlag)
	if err != nil {
		log.Fatalf("Failed to load configuration file (%s): %v", *configFlag, err)
	}

	logFile, err := logger.Setup(cfg.LogPath)
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	if logFile != nil {
		defer logFile.Close()
	}

	logger.Info("Orbitron daemon initializing...")
	cfg.SetupProxy()

	srv := server.NewServer(cfg)

	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.Start(); err != nil {
			logger.Fatal("Server error: %v", err)
		}
	}()

	sig := <-stopChan
	logger.Info("Received signal '%v'. Shutting down Orbitron daemon gracefully...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("Error during server shutdown: %v", err)
	}

	logger.Info("Orbitron daemon stopped cleanly.")
}
