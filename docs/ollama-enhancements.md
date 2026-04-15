# Ollama Enhancements (v0.6)

This document describes the enhanced Ollama integration capabilities added to aidev.

## Overview

The enhanced Ollama integration provides:

1. **Dynamic Model Management** - Automatic discovery and validation of available models
2. **Streaming Support** - Real-time response streaming for better user experience  
3. **Enhanced Error Handling** - Retry logic with exponential backoff and circuit breakers
4. **Intelligent Recommendations** - VRAM-aware model recommendations for different tiers
5. **Comprehensive CLI Tools** - Management commands for Ollama operations

## New CLI Commands

### `aidev ollama list`

Lists all available Ollama models with their capabilities:

```bash
$ aidev ollama list
Found 5 Ollama models:

  qwen2.5-coder:32b              Tools: Yes VRAM:    ~32GB  Size:  18.5 GB
  qwen2.5-coder:14b              Tools: Yes VRAM:    ~16GB  Size:   8.4 GB
  qwen2.5-coder:7b               Tools: Yes VRAM:     ~8GB  Size:   4.4 GB
```

### `aidev ollama recommend <tier> <vram>`

Recommends the best model for a given tier and available VRAM:

```bash
$ aidev ollama recommend medium 16GB
Recommended model for medium tier with 16GB VRAM: qwen2.5-coder:14b
  Tool support: true
  Streaming: true
  Estimated VRAM: ~16GB
```

### `aidev ollama validate <model> [--require-tools]`

Validates that a model exists and optionally supports tools:

```bash
$ aidev ollama validate qwen2.5-coder:14b --require-tools
Model qwen2.5-coder:14b is valid and supports tools
```

### `aidev ollama health`

Performs a comprehensive health check of the Ollama daemon:

```bash
$ aidev ollama health
Ollama daemon is healthy
  Available models: 5
  Largest model: qwen2.5-coder:32b
```

### `aidev ollama profile`

Shows current Ollama usage profile and recommendations:

```bash
$ aidev ollama profile
Ollama Usage Profile
==================
Status: HEALTHY
Models: 5 total (42.2 GB)
Tool-capable: 4/5
Streaming-capable: 5/5
Largest model: qwen2.5-coder:32b (18.5 GB)
```

## Enhanced Error Handling

### Retry Logic

All providers now include automatic retry with configurable backoff:

- **Linear**: Fixed delay between attempts
- **Exponential**: Delay doubles each attempt (default)
- **Fibonacci**: Delay follows Fibonacci sequence

```yaml
# Example retry configuration in models.yaml
retry_config:
  max_attempts: 5
  base_delay: 2s
  max_delay: 30s
  backoff: exponential
```

### Circuit Breaker

Cloud providers include circuit breaker protection to prevent cascading failures:

- Opens after 3 consecutive failures
- Stays open for 60 seconds
- Half-open state allows test requests

## Streaming Support

### Ollama Streaming

Ollama providers now support real-time streaming:

```go
if streamer, ok := provider.(llm.Streamer); ok {
    chunks, err := streamer.Stream(ctx, req)
    for chunk := range chunks {
        fmt.Print(chunk.Text)
        if chunk.Done {
            break
        }
    }
}
```

### Tool-Aware Streaming

Enhanced streaming for tool-use conversations maintains context across tool calls.

## Model Capabilities

### Tool-Capable Models

The following models support native tool use:

- qwen2.5, qwen2.5-coder (all sizes)
- llama3.1, llama3.2
- mistral-nemo
- command-r-plus
- firefunction-v2
- deepseek-coder
- codellama

### VRAM Estimation

Automatic VRAM estimation based on model size:

- 7B models: ~8GB VRAM
- 14B models: ~16GB VRAM  
- 32B models: ~32GB VRAM
- 70B models: ~70GB VRAM

## Integration with Existing Configuration

The enhanced Ollama integration is backward compatible with existing `models.yaml` files. Existing configurations will automatically benefit from:

- Retry logic (5 attempts for Ollama, 3 for cloud providers)
- Circuit breaker protection for cloud providers
- Streaming support when available

### Migration Path

No migration is required. Existing configurations continue to work unchanged. To enable new features:

1. Use `aidev ollama recommend` to find optimal models
2. Update `models.yaml` with recommended models
3. Use `aidev ollama validate` to verify configuration

## Performance Improvements

### Reduced Latency

- **Model warmup**: Pre-load commonly used models
- **Connection pooling**: Reuse HTTP connections
- **Timeout optimization**: Adaptive timeouts based on model size

### Resource Management

- **Memory monitoring**: Track Ollama daemon memory usage
- **VRAM awareness**: Prevent model loading beyond available VRAM
- **Graceful degradation**: Fall back to smaller models when needed

## Troubleshooting

### Common Issues

1. **"Ollama unreachable"**
   - Ensure Ollama is installed: `curl -fsSL https://ollama.com/install.sh | sh`
   - Start daemon: `ollama serve`
   - Check port: `curl http://localhost:11434/api/tags`

2. **"Model not found"**
   - Pull model: `ollama pull qwen2.5-coder:14b`
   - Verify with: `aidev ollama list`

3. **"Insufficient VRAM"**
   - Check available VRAM: `aidev ollama profile`
   - Use smaller model: `aidev ollama recommend small 8GB`

### Debug Mode

Enable debug logging with environment variable:

```bash
export AIDEV_DEBUG=1
aidev -issue https://github.com/owner/repo/issues/42 -repo ./my-repo
```

## Future Enhancements

Planned improvements for future releases:

1. **Model Caching** - Intelligent caching of model responses
2. **Load Balancing** - Distribute requests across multiple Ollama instances
3. **Model Quantization** - Automatic model optimization for available hardware
4. **GPU Selection** - Multi-GPU support and automatic GPU selection
5. **Metrics Collection** - Performance metrics and usage analytics
