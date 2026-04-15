package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/llm"
)

// runOllamaSubcommand handles `aidev ollama <subcommand>`
func runOllamaSubcommand() {
	if len(os.Args) < 2 {
		ollamaUsageAndExit()
	}
	sub := os.Args[1]
	os.Args = append(os.Args[:1], os.Args[2:]...)

	switch sub {
	case "list":
		runOllamaList()
	case "recommend":
		runOllamaRecommend()
	case "validate":
		runOllamaValidate()
	case "health":
		runOllamaHealth()
	case "profile":
		runOllamaProfile()
	default:
		fmt.Fprintf(os.Stderr, "Unknown ollama subcommand: %s\n", sub)
		ollamaUsageAndExit()
	}
}

func ollamaUsageAndExit() {
	fmt.Fprintf(os.Stderr, `Usage: aidev ollama <subcommand> [args]

Subcommands:
  list                    List all available Ollama models
  recommend <tier> <vram> Recommend best model for tier and VRAM
  validate <model>        Check if model exists and supports tools
  health                  Check Ollama daemon health
  profile                 Show current Ollama usage profile

Examples:
  aidev ollama list
  aidev ollama recommend medium 16GB
  aidev ollama validate qwen2.5-coder:14b
  aidev ollama health

`)
	os.Exit(1)
}

func runOllamaList() {
	manager := llm.NewOllamaManager("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	models, err := manager.ListModels(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing models: %v\n", err)
		os.Exit(1)
	}

	if len(models) == 0 {
		fmt.Println("No models found. Pull a model with 'ollama pull <model>'")
		return
	}

	fmt.Printf("Found %d Ollama models:\n\n", len(models))
	for _, model := range models {
		caps := manager.GetModelCapabilities(model.Name)
		
		toolStatus := "No"
		if caps.SupportsTools {
			toolStatus = "Yes"
		}
		
		vramStr := "Unknown"
		if caps.EstimatedVRAM > 0 {
			vramGB := caps.EstimatedVRAM / (1024 * 1024 * 1024)
			vramStr = fmt.Sprintf("~%dGB", vramGB)
		}

		fmt.Printf("  %-30s Tools: %-3s VRAM: %8s  Size: %8s\n",
			model.Name, toolStatus, vramStr, formatBytes(model.Size))
	}
	
	fmt.Println()
}

func runOllamaRecommend() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: aidev ollama recommend <tier> <vram>\n")
		fmt.Fprintf(os.Stderr, "  tier: small | medium | large\n")
		fmt.Fprintf(os.Stderr, "  vram: e.g., 8GB, 16GB, 32GB\n")
		os.Exit(1)
	}

	tier := os.Args[1]
	vramStr := os.Args[2]
	
	vram, err := parseVRAM(vramStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid VRAM format: %v\n", err)
		os.Exit(1)
	}

	manager := llm.NewOllamaManager("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	model, err := manager.RecommendModel(ctx, tier, vram)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error recommending model: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Recommended model for %s tier with %s VRAM: %s\n", tier, vramStr, model)
	
	// Show model details
	caps := manager.GetModelCapabilities(model)
	fmt.Printf("  Tool support: %v\n", caps.SupportsTools)
	fmt.Printf("  Streaming: %v\n", caps.SupportsStreaming)
	if caps.EstimatedVRAM > 0 {
		vramGB := caps.EstimatedVRAM / (1024 * 1024 * 1024)
		fmt.Printf("  Estimated VRAM: ~%dGB\n", vramGB)
	}
}

func runOllamaValidate() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: aidev ollama validate <model-name> [--require-tools]\n")
		os.Exit(1)
	}

	modelName := os.Args[1]
	requireTools := false
	if len(os.Args) > 2 && os.Args[2] == "--require-tools" {
		requireTools = true
	}

	manager := llm.NewOllamaManager("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := manager.ValidateModel(ctx, modelName, requireTools)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Validation failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Model %s is valid", modelName)
	if requireTools {
		fmt.Printf(" and supports tools")
	}
	fmt.Println()
}

func runOllamaHealth() {
	manager := llm.NewOllamaManager("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := manager.Health(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Health check failed: %v\n", err)
		fmt.Println("\nTroubleshooting:")
		fmt.Println("1. Ensure Ollama is installed: https://ollama.com")
		fmt.Println("2. Start Ollama daemon: ollama serve")
		fmt.Println("3. Check if running on correct port (default: 11434)")
		os.Exit(1)
	}

	fmt.Println("Ollama daemon is healthy")
	
	// Show additional info
	models, err := manager.ListModels(ctx)
	if err == nil {
		fmt.Printf("  Available models: %d\n", len(models))
		if len(models) > 0 {
			fmt.Printf("  Largest model: %s\n", findLargestModel(models))
		}
	}
}

func runOllamaProfile() {
	fmt.Println("Ollama Usage Profile")
	fmt.Println("==================")
	
	manager := llm.NewOllamaManager("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Check health
	err := manager.Health(ctx)
	if err != nil {
		fmt.Printf("Status: UNHEALTHY - %v\n", err)
		return
	}
	fmt.Println("Status: HEALTHY")

	// List models
	models, err := manager.ListModels(ctx)
	if err != nil {
		fmt.Printf("Error listing models: %v\n", err)
		return
	}

	if len(models) == 0 {
		fmt.Println("Models: None (pull a model with 'ollama pull')")
		return
	}

	// Analyze model capabilities
	var toolCapable, streamingCapable int
	var totalSize int64
	var largestModel string
	var largestSize int64

	for _, model := range models {
		caps := manager.GetModelCapabilities(model.Name)
		if caps.SupportsTools {
			toolCapable++
		}
		if caps.SupportsStreaming {
			streamingCapable++
		}
		
		totalSize += model.Size
		if model.Size > largestSize {
			largestSize = model.Size
			largestModel = model.Name
		}
	}

	fmt.Printf("Models: %d total (%s)\n", len(models), formatBytes(totalSize))
	fmt.Printf("Tool-capable: %d/%d\n", toolCapable, len(models))
	fmt.Printf("Streaming-capable: %d/%d\n", streamingCapable, len(models))
	fmt.Printf("Largest model: %s (%s)\n", largestModel, formatBytes(largestSize))

	// Recommendations
	fmt.Println("\nRecommendations:")
	if toolCapable < len(models)/2 {
		fmt.Println("- Consider pulling more tool-capable models for better Implementer performance")
	}
	if len(models) < 3 {
		fmt.Println("- Consider pulling models for different tiers (small, medium, large)")
	}
	
	fmt.Println("\nTool-capable models to consider:")
	fmt.Println("- qwen2.5-coder:7b (small tier, ~8GB VRAM)")
	fmt.Println("- qwen2.5-coder:14b (medium tier, ~16GB VRAM)")
	fmt.Println("- qwen2.5:32b (large tier, ~32GB VRAM)")
}

// Helper functions

func parseVRAM(vramStr string) (int64, error) {
	vramStr = strings.ToUpper(strings.TrimSpace(vramStr))
	
	if strings.HasSuffix(vramStr, "GB") {
		numStr := strings.TrimSuffix(vramStr, "GB")
		gb, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, err
		}
		return int64(gb * 1024 * 1024 * 1024), nil
	}
	
	if strings.HasSuffix(vramStr, "MB") {
		numStr := strings.TrimSuffix(vramStr, "MB")
		mb, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, err
		}
		return int64(mb * 1024 * 1024), nil
	}
	
	// Assume GB if no unit
	gb, err := strconv.ParseFloat(vramStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid VRAM format: %s", vramStr)
	}
	return int64(gb * 1024 * 1024 * 1024), nil
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func findLargestModel(models []*llm.ModelInfo) string {
	var largest *llm.ModelInfo
	for _, model := range models {
		if largest == nil || model.Size > largest.Size {
			largest = model
		}
	}
	if largest != nil {
		return largest.Name
	}
	return "unknown"
}
