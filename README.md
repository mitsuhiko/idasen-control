# idasen-control

[![npm version](https://img.shields.io/npm/v/idasen-control.svg)](https://www.npmjs.com/package/idasen-control)

This utility lets one control an IKEA IDÅSEN desk via command line utility.  It
spawns a small server to keep the connection to the desk alive on first use and
reuses that later.  This could be improved but is good enough for my purposes.

This lets you move to your preferred heights without having to keep the button
pressed.

It stores its configuration in a file called `~/.idasen-control.json`.

## Installation

If you have [volta](https://volta.sh/) then you can install it like this:

```
volta install idasen-control
```

Otherwise you can use npm:

```
npm install -g idasen-control
```

## Scanning

To find desks available you need to bring the tool into scanning mode after
setting your desk to pairing mode (keep bluetooth button pressed).  It will
do an initial scan for 10 seconds and wait for 2 more seconds after each
discovered device.

```
idasen-control --scan
```

## Connecting

After you know the address of the desk to connect to it:

```
idasen-control --connect-to ADDRESS
```

This will store the address in the config file.

## Check Desk Status

To check the status of the desk run:

```
idasen-control --status
```

## Move Desk

To move the desk to a position run this command:

```
idasen-control --move-to POSITION
```

For instance `10` is a typical sitting position, `50` is a typical standing
position.  If you want to block the shell until the desk is in its final
position pass `--wait`.

This is best used with shell aliases.

## License

It's based on Steven Roebert's desk-control MQTT script.

[MIT](./LICENSE)
