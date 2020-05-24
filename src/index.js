#!/usr/bin/env node
const process = require("process");
const { spawn } = require("child_process");
const fs = require("fs");
const net = require("net");
const { DeskManager } = require("./desk-manager");
const { program } = require("commander");
const { promisify } = require("util");
const { getIdleTime } = require("desktop-idle");
const prettyMilliseconds = require("pretty-ms");

const readFile = promisify(fs.readFile);
const writeFile = promisify(fs.writeFile);
const unlink = promisify(fs.unlink);

const SOCKET = "/tmp/idasen-control.sock";
const PIDFILE = "/tmp/idasen-control.pid";
const CHECK_INTERVAL = 5.0;
const BREAK_TIME = 2 * 60;
const STAND_THRESHOLD = 30;

program
  .option("-a, --addr <addr>", "Explicit bluetooth address of the table")
  .option("--position-max <number>", "Maximum position to be honored", (val) =>
    parseInt(val, 10)
  )
  .option("-m, --move-to <number>", "Position to move the desk to", (val) =>
    parseInt(val, 10)
  )
  .option("-s, --status", "Get the current status")
  .option("--prompt-fragment", "Render a prompt fragment")
  .option("--json", "Output as JSON")
  .parse(process.argv);

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function runClient() {
  if (!(await readPid())) {
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
    await sendCommand({ op: "moveTo", pos: program.moveTo });
  } else if (program.status) {
    const status = await sendCommand({ op: "getStatus" });
    if (program.json) {
      console.log(JSON.stringify(status));
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
    const status = await sendCommand({ op: "getStatus" });
    let vars = {
      sittingTime: prettyMilliseconds(status.sittingTime * 1000),
      sittingWarning:
        status.sittingTime >= 30 * 60
          ? `sitting for ${prettyMilliseconds(status.sittingTime * 1000)}`
          : "",
      positionHint: status.pos,
      standingHint: status.pos === "standing" ? "standing" : "",
      sittingHint: status.pos === "sitting" ? "sitting" : "",
    };
    console.log(
      template.replace(/%\((.*?)\)s/g, (_, group) => {
        return vars[group] || "";
      })
    );
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

function describePosition(desk) {
  return desk.position >= STAND_THRESHOLD ? "standing" : "sitting";
}

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

async function runServer() {
  let resolveReadyPromise = null;
  let readyPromise = new Promise((resolve) => {
    resolveReadyPromise = resolve;
  });
  let sittingTime = 0;

  const manager = new DeskManager({
    deskAddress: program.addr || process.env.IDASEN_ADDR,
    deskPositionMax: program.positionMax || 58,
    readyCallback: async (foundDesk) => {
      resolveReadyPromise(foundDesk);
    },
  });

  setInterval(() => {
    readyPromise.then((desk) => {
      // someone did something
      const idleTime = getIdleTime();
      if (idleTime < CHECK_INTERVAL && desk.position < STAND_THRESHOLD) {
        sittingTime += CHECK_INTERVAL;
      } else if (desk.position >= STAND_THRESHOLD || idleTime >= BREAK_TIME) {
        sittingTime = 0;
      }
    });
  }, CHECK_INTERVAL * 1000);

  ensureServer(async (message) => {
    if (message.op === "moveTo") {
      const desk = await readyPromise;
      await desk.moveTo(message.pos);
      return true;
    } else if (message.op === "wait") {
      await readyPromise;
      return true;
    } else if (message.op === "getStatus") {
      const desk = await readyPromise;
      return {
        height: desk.position,
        pos: describePosition(desk),
        sittingTime,
      };
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
}

if (process.env.IDASEN_START_SERVER === "1") {
  runServer();
} else {
  runClient().then(() => process.exit(0));
}
