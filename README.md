# Engram

A lightweight, fast, and extensible knowledge management tool for developers. Engram helps you capture, organize, and retrieve information directly from your terminal.

> Fork of [Gentleman-Programming/engram](https://github.com/Gentleman-Programming/engram)

## Features

- **Fast full-text search** across all your notes and chunks
- **Semantic chunking** for intelligent content organization
- **Plugin marketplace** for extending functionality
- **Git-friendly** storage format using compressed JSONL
- **CLI-first** workflow designed for developers

## Installation

### From source

```bash
git clone https://github.com/your-username/engram.git
cd engram
go build -o engram ./cmd/engram
sudo mv engram /usr/local/bin/
```

### Using Go install

```bash
go install github.com/your-username/engram/cmd/engram@latest
```

## Quick Start

```bash
# Initialize engram in your project
engram init

# Add a note or chunk of knowledge
engram add "Remember to handle edge cases in the parser"

# Search your knowledge base
engram search "parser"

# List all chunks
engram list

# Show a specific chunk
engram show <chunk-id>

# Delete a chunk you no longer need
engram delete <chunk-id>
```

## Configuration

Engram stores its data in `.engram/` at the root of your project:

```
.engram/
  manifest.json       # Index of all chunks
  chunks/             # Compressed JSONL chunk files
    *.jsonl.gz
```

> **Personal note:** I keep a global engram knowledge base in `~/.engram/` by symlinking it, which lets me share snippets across all my projects.

## Plugin Marketplace

Extend engram with plugins via the marketplace:

```bash
# Browse available plugins
engram plugin list

# Install a plugin
engram plugin install <plugin-name>
```

Plugin configurations are stored in `.claude-plugin/marketplace.json`.

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feat/amazing-feature`)
3. Commit your changes (`git commit -m 'feat: add amazing feature'`)
4. Push to the branch (`git push origin feat/amazing-feature`)
5. Open a Pull Request

Please check existing [issues](https://github.com/your-username/engram/issues) before opening a new one. Use the provided issue templates for bug reports.

## License

MIT License — see [LICENSE](LICENSE) for details.
