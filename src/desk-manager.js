const noble = require("@abandonware/noble");
const schedule = require("node-schedule");
const EventEmitter = require("events");

const { Desk } = require("./desk");
const { log } = require("./utils");

class DeskManager extends EventEmitter {
  constructor(config) {
    super();
    this.config = config;
    this.started = false;
    this.connecting = false;
    this.desk = null;
    this._createReadyPromise();
  }

  _createReadyPromise() {
    this._deskReadyPromise = new Promise((resolve) => {
      this._deskReadyPromiseResolve = resolve;
    });
  }

  async getDesk() {
    await this._deskReadyPromise;
    return this.desk;
  }

  start() {
    this.startNoble();
  }

  log(...args) {
    if (this.config.verbose) {
      log(...args);
    }
  }

  startNoble() {
    this.log("starting BLE");
    noble.on("discover", async (peripheral) => {
      await this.processPeripheral(peripheral);
    });

    noble.on("stateChange", async (state) => {
      this.log("stateChange", state);
      if (state === "poweredOn") {
        await this.scan();
      } else {
        if (this.desk) {
          this.desk.disconnect();
        }
        this.desk = null;
        this._createReadyPromise();
        this.didUpdateDevice();
      }
    });

    noble.on("scanStop", async () => {
      this.log("scanStop");
      if (!this.desk && noble.state == "poweredOn") {
        this.scan();
      }
    });
  }

  async scan() {
    if (this.desk) {
      return;
    }

    this.log("Starting scan");
    try {
      await noble.startScanningAsync([], true);
    } catch (err) {
      this.scheduleScan();
    }
  }

  scheduleScan() {
    schedule.scheduleJob(Date.now() + 5000, () => {
      if (noble.state == "poweredOn") {
        this.scan();
      }
    });
  }

  isDeskPeripheral(peripheral) {
    if (peripheral.address == this.config.deskAddress) {
      return true;
    }

    if (!peripheral.advertisement || !peripheral.advertisement.serviceUuids) {
      return false;
    }

    return peripheral.advertisement.serviceUuids.includes(
      Desk.services().control.id
    );
  }

  async processPeripheral(peripheral) {
    if (this.desk || !this.isDeskPeripheral(peripheral)) {
      return;
    }

    this.emit("discover", peripheral);

    await this.ensureAddressKnown(peripheral);
    if (peripheral.address === this.config.deskAddress) {
      this.log("Found configured desk", peripheral.address);
      this.desk = new Desk(peripheral, this.config.deskPositionMax);
      peripheral.on("disconnect", () => {
        this.log("desk disconnected, going back to scanning");
        this.desk = null;
        this._createReadyPromise();
        this.scan();
      });

      try {
        await noble.stopScanningAsync();
      } catch (err) {
        // We don't really care
      }

      this.didUpdateDevice();
    } else {
      this.log("Discovered a desk at", peripheral.address);
    }
  }

  /**
   * On MacOS, the peripheral address is unknown until it is being connected to.
   * By connecting to it once, we work around this issue.
   *
   * @see https://github.com/mitsuhiko/idasen-control/issues/3
   * @see https://github.com/abandonware/noble#event-peripheral-discovered
   * @return void
   */
  async ensureAddressKnown(peripheral) {
    if (this.connecting) {
      return
    }

    if (peripheral.address === '' && peripheral.addressType === 'unknown') {
      this.connecting = true;

      await peripheral.connectAsync();
      await peripheral.disconnectAsync();

      this.connecting = false;
    }
  }

  didUpdateDevice() {
    if (this.desk) {
      this.desk.on("position", async () => {
        if (!this.started) {
          this.started = true;
          this._deskReadyPromiseResolve();
        }
        this.emit("position", this.desk.position);
      });
    }
  }
}

module.exports.DeskManager = DeskManager;
