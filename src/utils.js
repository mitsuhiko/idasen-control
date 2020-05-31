module.exports.sleep = function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
};

module.exports.log = function log(...message) {
  console.log(`[${new Date().toISOString()}]`, ...message);
};
