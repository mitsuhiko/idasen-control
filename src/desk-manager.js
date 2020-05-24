const noble = require("@abandonware/noble");
const schedule = require("node-schedule");
const { Desk } = require("./desk");

class DeskManager {
  constructor(config) {
    this.config = config;
    this.started = false;
  }

  start() {
    this.startNoble();
  }

  startNoble() {
    noble.on("discover", async (peripheral) => {
      await this.processPeripheral(peripheral);
    });

    noble.on("stateChange", async (state) => {
      if (state === "poweredOn") {
        await this.scan();
      } else {
        if (this.desk) {
          this.desk.disconnect();
        }
        this.desk = null;
        this.didUpdateDevice();
      }
    });

    noble.on("scanStop", async () => {
      if (!this.desk && noble.state == "poweredOn") {
        this.scan();
      }
    });
  }

  async scan() {
    if (this.desk) {
      return;
    }

    try {
      await noble.startScanningAsync();
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

    this.desk = new Desk(peripheral, this.config.deskPositionMax);

    try {
      await noble.stopScanningAsync();
    } catch (err) {
      // We don't really care
    }

    this.didUpdateDevice();
  }

  didUpdateDevice() {
    if (this.desk) {
      this.desk.on("position", async () => {
        if (!this.started) {
          this.started = true;
          if (this.config.readyCallback) {
            await this.config.readyCallback(this.desk);
          }
        }
        if (this.started && this.config.positionCallback) {
          await this.config.positionCallback(this.desk);
        }
      });
    }
  }
}

module.exports.DeskManager = DeskManager;
