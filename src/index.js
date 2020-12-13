#!/usr/bin/env node
const process = require("process");
const { spawn } = require("child_process");
const fs = require("fs");
const net = require("net");
const { program } = require("commander");
const { promisify } = require("util");
const prettyMilliseconds = require("pretty-ms");

const { DeskManager } = require("./desk-manager");
const { log, sleep } = require("./utils");
const { getConfig, loadConfig, saveConfig } = require("./config");

const readFile = promisify(fs.readFile);
const writeFile = promisify(fs.writeFile);
const unlink = promisify(fs.unlink);

const CHECK_INTERVAL = 5.0;

program
  .option("-m, --move-to <number>", "Position to move the desk to", (val) =>
    parseInt(val, 10)
  )
  .option("-s, --status", "Get the current status")
  .option("--wait", "Waits for operations to finish")
  .option("--connect-to <ADDR>", "Connect to the desk with the given address")
  .option("--prompt-fragment", "Render a prompt fragment")
  .option("--print-config", "Print the config")
  .option("--json", "Output as JSON")
  .option("--scan", "Scans for desks")
  .option("--server", "Starts the server (done automatically normally)")
  .option("--stop-server", "Stops the server if it's running")
  .parse(process.argv);

async function runClient() {
  const config = getConfig();

  // non server operations
  if (program.scan) {
    console.log("Scanning for desks");
    const manager = new DeskManager({
      verbose: false,
    });
    let resolveDonePromise;
    const donePromise = new Promise((resolve) => {
      resolveDonePromise = resolve;
    });
    let scanUntil = +new Date() + 10000;
    let found = 0;
    setInterval(() => {
      if (scanUntil < new Date()) {
        resolveDonePromise();
      }
    }, 1000);

    let seen = {};
    manager.on("discover", (peripheral) => {
      if (
        peripheral.address &&
        peripheral.advertisement.localName &&
        !seen[peripheral.id]
      ) {
        seen[peripheral.id] = peripheral;
        console.log(
          `  Found "${peripheral.advertisement.localName}" [address: ${peripheral.address}]`
        );
        found++;
        scanUntil = +new Date() + 2000;
      }
    });
    manager.start();

    await donePromise;
    console.log("Done scanning.");
    if (found > 0) {
      console.log(
        `Found ${found} desk${
          found == 1 ? "" : "s"
        }.  Connect with --connect-to`
      );
    } else {
      console.log(
        "No desks found. Make sure to bring the desk to pairing mode before scanning."
      );
    }
  } else if (program.printConfig) {
    if (program.json) {
      console.log(JSON.stringify(config, null, 2));
    } else {
      console.log(config);
    }
  } else if (program.connectTo) {
    config.deskAddress = program.connectTo;
    await saveConfig();
  } else if (program.stopServer) {
    const pid = await readPid();
    if (pid !== null) {
      console.log("Stopping server");
      process.kill(pid);
    } else {
      console.log("Server not running");
    }
  } else {
    // these operations want the server.
    if (!(await serverIsRunning())) {
      if (process.env.IDASEN_NO_DAEMON === "1") {
        runServer();
      } else {
        const env = { ...process.env, IDASEN_START_SERVER: "1" };
        const [_first, ...argv] = process.argv;
        spawn(process.execPath, argv, {
          env,
          detached: true,
          stdio: "ignore",
        });
      }
      await sleep(100);
    }

    if (program.moveTo) {
      await sendCommand({ op: "moveTo", pos: program.moveTo }, program.wait);
    } else if (program.status) {
      const status = await getStatus();
      if (program.json) {
        console.log(JSON.stringify(status, null, 2));
      } else if (!status.ready) {
        console.log("Desk is not ready");
      } else {
        console.log(`height: ${status.height} (${status.pos})`);
        console.log(
          `time sitting: ${prettyMilliseconds(status.sittingTime * 1000)}`
        );
      }
    } else if (program.promptFragment) {
      const template =
        process.env.IDASEN_PROMPT_TEMPLATE ||
        "%(sittingWarning)s %(standingHint)s";
      const status = await getStatus();
      const sittingTime = status.ready ? status.sittingTime : 0;
      const pos = status.ready ? status.pos : "unknown";
      let vars = {
        sittingTime: prettyMilliseconds(sittingTime * 1000),
        sittingWarning:
          sittingTime >= 30 * 60
            ? `sitting for ${prettyMilliseconds(sittingTime * 1000)}`
            : "",
        positionHint: pos,
        standingHint: pos === "standing" ? "standing" : "",
        sittingHint: pos === "sitting" ? "sitting" : "",
      };
      console.log(
        template.replace(/%\((.*?)\)s/g, (_, group) => {
          return vars[group] || "";
        })
      );
    }
  }
}

async function serverIsRunning() {
  return (await readPid()) !== null;
}

async function readPid() {
  const config = getConfig();
  try {
    const contents = await readFile(config.pidFilePath, "utf8");
    const pid = parseInt(contents, 10);
    if (Number.isNaN(pid)) {
      return null;
    }
    try {
      if (process.kill(pid, 0)) {
        return pid;
      }
    } catch (e) {
      if (e.code === "EPERM") {
        console.log("eperm");
        return pid;
      }
    }
  } catch (e) {
    // ignore
  }
  return null;
}

async function writePid() {
  await writeFile(getConfig().pidFilePath, `${process.pid}\n`);
}

async function ensureServer(onMessage) {
  const config = getConfig();

  try {
    await unlink(config.socketPath);
  } catch (e) {
    // doesn't matter
  }

  const server = net
    .createServer((stream) => {
      let buffer = "";
      let connected = true;
      stream.on("data", async (data) => {
        buffer += data;
        while (true) {
          const newline = buffer.indexOf("\n");
          if (newline < 0) {
            break;
          }
          let parsedMsg;
          try {
            let msg = buffer.substr(0, newline);
            buffer = buffer.substr(newline + 1);
            parsedMsg = JSON.parse(msg);
          } catch (e) {
            continue;
          }

          log("received request", parsedMsg);
          let rv = await onMessage(parsedMsg);
          if (connected) {
            log("sending response", rv);
            stream.write(JSON.stringify(rv) + "\n");
          } else {
            log("dropping response because client disconnected");
          }
        }
      });
      stream.on("end", () => {
        connected = false;
      });
    })
    .listen(config.socketPath);

  await writePid();

  return server;
}

process.on("SIGINT", () => {
  process.exit();
});

function describePosition(desk) {
  return desk.position >= getConfig().standThreshold ? "standing" : "sitting";
}

async function sendCommand(cmd, wait) {
  wait = wait || false;
  const config = getConfig();
  return new Promise((resolve) => {
    const client = net.createConnection({ path: config.socketPath }, () => {
      client.write(JSON.stringify(cmd) + "\n", () => {
        if (!wait) {
          resolve(undefined);
        }
      });
    });
    if (wait) {
      client.on("data", (data) => {
        resolve(JSON.parse(data));
      });
    }
    client.on("end", () => {
      // nothing
    });
  });
}

async function getStatus() {
  const status = await Promise.race([
    sendCommand({ op: "getStatus" }, true),
    sleep(100),
  ]);
  return status || { ready: false };
}

async function runServer() {
  const config = getConfig();
  let sittingTime = 0;

  const manager = new DeskManager({
    deskAddress: config.deskAddress,
    deskPositionMax: config.deskPositionMax || 58,
    verbose: true,
  });

  ensureServer(async (message) => {
    if (message.op === "moveTo") {
      const desk = await manager.getDesk();
      await desk.moveTo(message.pos);
      return true;
    } else if (message.op === "wait") {
      await manager.getDesk();
      return true;
    } else if (message.op === "getStatus") {
      const desk = await Promise.race([manager.getDesk(), sleep(50)]);
      if (!desk) {
        return { ready: false };
      }
      return {
        ready: true,
        height: desk.position,
        pos: describePosition(desk),
        sittingTime,
      };
    } else {
      log("unknown message, ignoring");
      return false;
    }
  }).then(() => {
    manager.start();
  });

  process.on("exit", () => {
    try {
      fs.unlinkSync(config.pidFilePath);
    } catch (e) {
      // ignore
    }
    try {
      fs.unlinkSync(config.socketPath);
    } catch (e) {
      // ignore
    }
  });
}

async function main() {
  await loadConfig();
  if (program.server || process.env.IDASEN_START_SERVER === "1") {
    runServer();
  } else {
    runClient().then(() => process.exit(0));
  }
}

main();
