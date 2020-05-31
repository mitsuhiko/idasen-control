const fs = require("fs");
const { homedir } = require("os");
const { promisify } = require("util");

const readFile = promisify(fs.readFile);
const writeFile = promisify(fs.writeFile);

function getConfigPath() {
  return homedir() + "/.idasen-control.json";
}

function getDefaultConfig() {
  return {
    socketPath: "/tmp/idasen-control.sock",
    pidFilePath: "/tmp/idasen-control.pid",
    standThreshold: 30,
    sittingBreakTime: 2 * 60,
    deskAddress: null,
    deskMaxPosition: 58,
    connectTimeout: 5.0,
  };
}

let cachedConfig = null;

module.exports.loadConfig = async function loadConfig() {
  const path = getConfigPath();
  let config = getDefaultConfig();
  try {
    config = { ...config, ...JSON.parse(await readFile(path)) };
  } catch (e) {
    // ignore load errors
  }

  cachedConfig = config;
};

module.exports.getConfig = function getConfig() {
  return cachedConfig;
};

module.exports.saveConfig = async function saveConfig() {
  await writeFile(
    getConfigPath(),
    JSON.stringify(await module.exports.getConfig(), null, 2)
  );
};
