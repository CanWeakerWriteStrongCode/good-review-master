# "Not Good Review Master" QQ Group Bot

A QQ group chat bot that listens to messages via NapCatQQ and triggers
AI-generated responses (sharp reviews, Q&A, etc.) through keyword matching.

## Quick Start

1. Copy config_example.yaml to config.yaml and fill in your settings:

   cp config_example.yaml config.yaml

2. Edit config.yaml:

   - napcat.http_api: NapCatQQ HTTP API address (default http://127.0.0.1:3000)
   - napcat.access_token: NapCatQQ access token (set in NapCatQQ WebUI)
   - bot.qq: Your bot's QQ number
   - bot.allow_groups: Allowed group IDs (comma-separated)
   - llm.api_key: Your LLM API key
   - llm.api_base: LLM API base URL
   - llm.model_name: Model name

3. Run from source:

   Windows: double-click start.bat
   Linux:   ./start.sh

   Package as executable:

   Windows: double-click build.bat
   Linux:   ./build.sh

## Requirements

- Go 1.25+
- NapCatQQ running locally
- An OpenAI-compatible LLM API (DeepSeek, etc.)

## Project Structure

  main.go          - Entry point
  config/          - Configuration loader
  cache/           - Message ring buffer cache
  llm/             - LLM client (OpenAI-compatible)
  onebot/          - NapCatQQ HTTP API client
  bot/             - Message filtering + polling loop
  cmd/             - Command router + handlers

## Adding New Commands

1. Add config under "cmd:" in config.yaml
2. Create handler file in cmd/
3. Register route in cmd/router.go
