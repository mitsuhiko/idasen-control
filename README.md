# desk

Minimal IKEA IDÅSEN control tool written in Rust using [`btleplug`](https://crates.io/crates/btleplug).

## Features

- Scan for nearby desks
- Connect once and store the selected desk in `~/.idasen.json`
- Move desk up/down
- Move desk to an explicit height
- Read current desk height
- Auto-managed background daemon for faster repeated commands

## Requirements

- Rust toolchain (stable)
- Bluetooth LE support on your system

## Build

```bash
cargo build --release
```

Binary:

```bash
./target/release/desk
```

## Usage

```bash
desk list
desk connect [IDENTIFIER]
desk up
desk down
desk move-to <HEIGHT>
desk status
```

You can also run without installing:

```bash
cargo run -- <COMMAND>
```

Examples:

```bash
cargo run -- list
cargo run -- connect
cargo run -- connect "cc70b52f-b68e-356b-45f0-a401bed542ce"
cargo run -- status
cargo run -- move-to 45
```

## Configuration

Configuration is stored in:

```text
~/.idasen.json
```

Default bounds:

- `position_min`: `0.0`
- `position_max`: `58.0`

`up`, `down`, `move-to`, and `status` use a background daemon that starts on demand and exits after 30 minutes of inactivity.

## Troubleshooting

For debugging connection or timeout issues:

```bash
./target/release/desk --verbose --timeout 6 --connect-timeout 6 connect <IDENTIFIER>
```