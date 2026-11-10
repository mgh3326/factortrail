# Factortrail

Factortrail (팩터트레일) is a Go service for investment data, analysis, and research, with traceable evidence and reproducible datasets.

투자 데이터의 출처와 분석 근거를 이어 주는 서비스.

## Status

P0 provides eight read-only MCP tools backed by the `retro/v1` HTTP API of [auto_trader](https://github.com/mgh3326/auto_trader). Factortrail does not read a database directly and has no write or order path.

This build serves the `retro` profile with the eight tools, behind Cloudflare Access authentication, through a client generated from a pinned copy of the auto_trader contract. See [docs/service.md](docs/service.md).

Later capabilities may own data quality, datasets, and analysis when their value, source of truth, validation, migration, and rollback are established.

## Data policy

This repository is public. It holds code and fully synthetic test data only. Real account, amount, or holdings data, including anonymized copies, never enters this repository.

## License

MIT
