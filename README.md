# idasen-control

This utility lets one control an IKEA IDÅSEN desk via command line utility.  It
spawns a small server to keep the connection to the desk alive on first use and
reuses that later.  This could be improved but is good enough for my purposes.

This lets you move to your preferred heights without having to keep the button
pressed.

## Installation

```
npm install -g idasen-control
```

or your prefered way to manage `npm` dependencies.

## Usage

```
export IDASEN_ADDR=address-of-the-desk

idasen-control --move-to HEIGHT
idasen-control --get-pos
idasen-control --status
idasen-control --json
idasen-control --addr <addr>
```

This is best to be used together with some shell aliases.

It's based on Steven Roebert's desk-control MQTT script.


## License

[MIT](./LICENSE)
