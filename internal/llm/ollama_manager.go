package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ModelInfo represents information about an available Ollama model
type ModelInfo struct {
	Name       string       `json:"name"`
	Size       int64        `json:"size"`
	Digest     string       `json:"digest"`
	ModifiedAt time.Time    `json:"modified_at"`
	Details    ModelDetails `json:"details"`
}

type ModelDetails struct {
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

// ModelCapabilities tracks what a model can do
type ModelCapabilities struct {
	SupportsTools     bool
	SupportsStreaming bool
	EstimatedVRAM     int64 // in bytes
}

// OllamaManager handles dynamic model discovery and management
type OllamaManager struct {
	endpoint   string
	http       *http.Client
	cache      map[string]*ModelInfo
	cacheTTL   time.Duration
	lastUpdate time.Time
}

// NewOllamaManager creates a new model manager
func NewOllamaManager(endpoint string) *OllamaManager {
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	return &OllamaManager{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 30 * time.Second},
		cache:    make(map[string]*ModelInfo),
		cacheTTL: 5 * time.Minute,
	}
}

// ListModels returns all available models from Ollama
func (om *OllamaManager) ListModels(ctx context.Context) ([]*ModelInfo, error) {
	// Check cache first
	if time.Since(om.lastUpdate) < om.cacheTTL && len(om.cache) > 0 {
		models := make([]*ModelInfo, 0, len(om.cache))
		for _, info := range om.cache {
			models = append(models, info)
		}
		return models, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, om.endpoint+"/api/tags", nil)
	if err != nil {
		return nil, err
	}

	resp, err := om.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama manager: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ollama manager: %s", resp.Status)
	}

	var result struct {
		Models []ModelInfo `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama manager decode: %w", err)
	}

	// Update cache
	om.cache = make(map[string]*ModelInfo)
	for i := range result.Models {
		model := &result.Models[i]
		om.cache[model.Name] = model
	}
	om.lastUpdate = time.Now()

	models := make([]*ModelInfo, len(result.Models))
	for i := range result.Models {
		models[i] = &result.Models[i]
	}

	return models, nil
}

// GetModelCapabilities determines what a model can do based on its name and details
func (om *OllamaManager) GetModelCapabilities(modelName string) *ModelCapabilities {
	// Known tool-capable models as of current Ollama versions
	toolCapableModels := map[string]bool{
		"qwen2.5":         true,
		"qwen2.5-coder":   true,
		"llama3.1":        true,
		"llama3.2":        true,
		"mistral-nemo":    true,
		"command-r-plus":  true,
		"firefunction-v2": true,
		"deepseek-coder":  true,
		"codellama":       true,
	}

	// Estimate VRAM requirements based on parameter size
	vramEstimates := map[string]int64{
		"7b":  8 * 1024 * 1024 * 1024,  // 8GB
		"14b": 16 * 1024 * 1024 * 1024, // 16GB
		"32b": 32 * 1024 * 1024 * 1024, // 32GB
		"70b": 70 * 1024 * 1024 * 1024, // 70GB
	}

	caps := &ModelCapabilities{
		SupportsStreaming: true, // Most Ollama models support streaming
	}

	// Check tool support
	for baseName := range toolCapableModels {
		if strings.Contains(modelName, baseName) {
			caps.SupportsTools = true
			break
		}
	}

	// Estimate VRAM
	for size, vram := range vramEstimates {
		if strings.Contains(modelName, size) {
			caps.EstimatedVRAM = vram
			break
		}
	}

	return caps
}

// RecommendModel suggests the best model for a given tier based on available models
func (om *OllamaManager) RecommendModel(ctx context.Context, tier string, availableVRAM int64) (string, error) {
	models, err := om.ListModels(ctx)
	if err != nil {
		return "", err
	}

	// Filter models by VRAM constraints
	var suitableModels []string
	for _, model := range models {
		caps := om.GetModelCapabilities(model.Name)
		if caps.EstimatedVRAM <= availableVRAM {
			suitableModels = append(suitableModels, model.Name)
		}
	}

	if len(suitableModels) == 0 {
		return "", fmt.Errorf("no models available within %d VRAM constraint", availableVRAM)
	}

	// Sort by preference based on tier
	switch tier {
	case "small":
		return om.selectBestModel(suitableModels, []string{"7b", "8b", "3b", "1b"})
	case "medium":
		return om.selectBestModel(suitableModels, []string{"14b", "13b", "12b", "8b"})
	case "large":
		return om.selectBestModel(suitableModels, []string{"32b", "34b", "70b", "14b"})
	default:
		if len(suitableModels) > 0 {
			return suitableModels[0], nil
		} else {
			return "", fmt.Errorf("no models available")
		}
	}
}

func (om *OllamaManager) selectBestModel(candidates []string, preferences []string) (string, error) {
	if len(candidates) == 0 {
		return "", fmt.Errorf("no candidates provided")
	}

	// Create a scoring system based on preferences
	scored := make(map[string]int)
	for _, pref := range preferences {
		for i, model := range candidates {
			if strings.Contains(model, pref) {
				scored[model] = len(preferences) - i // Higher score for better match
			}
		}
	}

	// Sort by score and return the best
	if len(scored) == 0 {
		return candidates[0], nil
	}

	type modelScore struct {
		name  string
		score int
	}

	var ranked []modelScore
	for name, score := range scored {
		ranked = append(ranked, modelScore{name, score})
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	return ranked[0].name, nil
}

// ValidateModel checks if a model exists and is tool-capable
func (om *OllamaManager) ValidateModel(ctx context.Context, modelName string, requireTools bool) error {
	models, err := om.ListModels(ctx)
	if err != nil {
		return err
	}

	// Check if model exists
	var found bool
	for _, model := range models {
		if model.Name == modelName {
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("model %q not found", modelName)
	}

	// Check tool capability if required
	if requireTools {
		caps := om.GetModelCapabilities(modelName)
		if !caps.SupportsTools {
			return fmt.Errorf("model %q does not support tools", modelName)
		}
	}

	return nil
}

// Health performs a comprehensive health check
func (om *OllamaManager) Health(ctx context.Context) error {
	// Basic connectivity
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, om.endpoint+"/api/tags", nil)
	if err != nil {
		return err
	}

	resp, err := om.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama unreachable at %s: %w", om.endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("ollama returned %s", resp.Status)
	}

	// Try to list models to ensure API is working
	_, err = om.ListModels(ctx)
	return err
}
