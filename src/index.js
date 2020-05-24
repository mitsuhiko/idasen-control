#!/usr/bin/env node
const process = require("process");
const { spawn } = require("child_process");
const fs = require("fs");
const net = require("net");
const { DeskManager } = require("./desk-manager");
const { program } = require("commander");
const { promisify } = require("util");

const readFile = promisify(fs.readFile);
const writeFile = promisify(fs.writeFile);
const unlink = promisify(fs.unlink);

const SOCKET = "/tmp/idasen-control.sock";
const PIDFILE = "/tmp/idasen-control.pid";

program
  .option("-a, --addr <addr>", "Explicit bluetooth address of the table")
  .option("--position-max <number>", "Maximum position to be honored", (val) =>
    parseInt(val, 10)
  )
  .option("-m, --move-to <number>", "Position to move the desk to", (val) =>
    parseInt(val, 10)
  )
  .option("-p, --get-pos", "Get the current position")
  .parse(process.argv);

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function runClient() {
  if (!(await readPid())) {
    const env = { ...process.env, IDASEN_START_SERVER: "1" };
    const [_first, ...argv] = process.argv;
    spawn(process.execPath, argv, {
      env,
      detached: true,
      stdio: "ignore",
    });
    await sleep(100);
  }

  if (program.moveTo) {
    console.log(await sendCommand({ op: "moveTo", pos: program.moveTo }));
  } else if (program.getPos) {
    console.log(await sendCommand({ op: "getPos" }));
  }
}

async function readPid() {
  try {
    const contents = await readFile(PIDFILE, "utf8");
    const pid = parseInt(contents, 10);
    try {
      return process.kill(pid, 0);
    } catch (e) {
      return e.code === "EPERM";
    }
  } catch (e) {
    return false;
  }
}

async function writePid() {
  await writeFile(PIDFILE, `${process.pid}\n`);
}

async function ensureServer(onMessage) {
  try {
    await unlink(SOCKET);
  } catch (e) {
    // doesn't matter
  }

  const server = net
    .createServer((stream) => {
      let buffer = "";
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

          let rv = await onMessage(parsedMsg);
          stream.write(JSON.stringify(rv) + "\n");
        }
      });
    })
    .listen(SOCKET);

  await writePid();

  return server;
}

process.on("SIGINT", () => {
  process.exit();
});

async function sendCommand(cmd) {
  return new Promise((resolve) => {
    const client = net.createConnection({ path: SOCKET }, () => {
      client.write(JSON.stringify(cmd) + "\n");
    });
    client.on("data", (data) => {
      resolve(JSON.parse(data));
    });
    client.on("end", () => {
      // nothing
    });
  });
}

if (process.env.IDASEN_START_SERVER === "1") {
  let resolveReadyPromise = null;
  let readyPromise = new Promise((resolve) => {
    resolveReadyPromise = resolve;
  });

  const manager = new DeskManager({
    deskAddress: program.addr || process.env.IDASEN_ADDR,
    deskPositionMax: program.positionMax || 58,
    readyCallback: async (foundDesk) => {
      resolveReadyPromise(foundDesk);
    },
  });

  ensureServer(async (message) => {
    if (message.op === "moveTo") {
      const desk = await readyPromise;
      await desk.moveTo(message.pos);
      return true;
    } else if (message.op === "wait") {
      await readyPromise;
      return true;
    } else if (message.op === "getPos") {
      const desk = await readyPromise;
      return desk.position;
    } else {
      return false;
    }
  }).then(() => {
    manager.start();
  });

  process.on("exit", () => {
    try {
      fs.unlinkSync(PIDFILE);
    } catch (e) {
      // ignore
    }
    try {
      fs.unlinkSync(SOCKET);
    } catch (e) {
      // ignore
    }
  });
} else {
  runClient().then(() => process.exit(0));
}
