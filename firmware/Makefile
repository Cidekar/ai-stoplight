# Makefile - build, flash and monitor the Stoplight firmware.
#
#   make          list the targets
#   make setup    install the ESP32 core and U8g2 (first run, a few hundred MB)
#   make flash    compile and upload to the attached board
#   make monitor  open the serial console at 115200
#   make test     run the host test suite (no board needed)
#
# This file exists to replace a click-through in the Arduino IDE. The board
# settings that matter are not defaults, and one of them (USB CDC On Boot) is
# silently wrong far more often than it is right. Encoding them in an FQBN
# makes them reviewable, repeatable and impossible to forget. See
# firmware/readme.md, "Flashing".
#
# The root Makefile builds and tests the Go relay. This one only ever touches
# the firmware, and shares no target name with it that means something else:
# `test` here is the host C++ suite, `clean` here removes the firmware build
# directories. Run them from their own directory.

# --- Configuration -----------------------------------------------------
#
# Every one of these is overridable on the command line:
#
#   make flash PORT=/dev/cu.usbmodem1101
#   make compile FQBN=esp32:esp32:esp32c3:CDCOnBoot=cdc,FlashSize=8M

ARDUINO_CLI ?= arduino-cli
SKETCH      ?= stoplight
BUILD_DIR   ?= build
TEST_DIR    ?= test
BAUD        ?= 115200

# The board manager URL for the Espressif core. Not shipped with arduino-cli.
ESP32_INDEX ?= https://espressif.github.io/arduino-esp32/package_esp32_index.json

# The ESP32-C3 SuperMini enumerates as a plain ESP32C3 Dev Module. The three
# menu options are the ones that are NOT correct by default:
#
#   CDCOnBoot=cdc   "USB CDC On Boot: Enabled". THE setting people miss in the
#                   GUI, and the whole reason a CLI flow is better here. With
#                   it at its default (Disabled) the board still enumerates and
#                   still uploads, so nothing looks wrong -- but Serial goes to
#                   the hardware UART pins instead of USB. The monitor stays
#                   empty, no frames ever arrive, and the device looks dead.
#   FlashSize=4M    4MB (32Mb), what this module carries.
#   FlashMode=qio   Quad I/O. Faster boot than dio.
#
# Menu keys and option values are verbatim from the esp32 core's boards.txt
# (esp32c3.menu.CDCOnBoot.cdc, .FlashSize.4M, .FlashMode.qio). Confirm them
# for your installed core with:  make board-details
FQBN ?= esp32:esp32:esp32c3:CDCOnBoot=cdc,FlashSize=4M,FlashMode=qio

# Auto-detect the port: first /dev/cu.usbmodem*, overridable with PORT=.
# Deferred (=) rather than immediate (:=) so that unplugging and replugging
# between two make invocations is picked up, and so `make help` and `make test`
# never pay for the glob.
PORT ?= $(firstword $(wildcard /dev/cu.usbmodem*))

.DEFAULT_GOAL := help

# --- Guards ------------------------------------------------------------
#
# Both guards are recipe prefixes rather than prerequisites, because a
# prerequisite target cannot abort the target that depends on it without
# also being rebuilt on every run.

# arduino-cli is not installed by this Makefile and never will be. Installing a
# toolchain is the user's call, so the guard prints the command and stops.
define require_cli
	@command -v $(ARDUINO_CLI) >/dev/null 2>&1 || { \
		echo "error: $(ARDUINO_CLI) is not installed."; \
		echo; \
		echo "  Install it with:"; \
		echo "      brew install arduino-cli"; \
		echo; \
		echo "  Then run 'make setup' to fetch the ESP32 core and U8g2."; \
		echo "  Nothing here installs anything on your behalf."; \
		exit 1; \
	}
endef

# No port is almost always a cable, not a board. A charge-only USB-C cable
# enumerates nothing at all and looks identical to a data one, so it is named
# first and by name.
define require_port
	@test -n "$(PORT)" || { \
		echo "error: no serial port found (looked for /dev/cu.usbmodem*)."; \
		echo; \
		echo "  Most likely, in order:"; \
		echo "    1. A CHARGE-ONLY USB-C cable. It enumerates nothing and looks"; \
		echo "       identical to a data cable. This is the usual cause."; \
		echo "    2. The board is not plugged in."; \
		echo "    3. The board is not in a state that enumerates. Hold BOOT,"; \
		echo "       tap RESET, release BOOT, then try again."; \
		echo; \
		echo "  Ports currently present:"; \
		ls /dev/cu.* 2>/dev/null | sed 's/^/      /' || echo "      (none)"; \
		echo; \
		echo "  Override the detection with:  make $@ PORT=/dev/cu.yourport"; \
		exit 1; \
	}
endef

# --- Targets -----------------------------------------------------------

.PHONY: help
help: ## List the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "  FQBN  $(FQBN)"
	@echo "  PORT  $(if $(PORT),$(PORT),(none detected - see 'make upload'))"

# Only the arduino-cli-internal steps. The CLI itself must already exist:
# the guard above prints the brew command rather than running it.
.PHONY: setup
setup: ## Install the ESP32 core and U8g2 (first run downloads a few hundred MB)
	$(require_cli)
	@echo "==> arduino-cli config"
	@$(ARDUINO_CLI) config init 2>/dev/null || true
	@$(ARDUINO_CLI) config add board_manager.additional_urls $(ESP32_INDEX) 2>/dev/null || true
	@echo "==> core update-index"
	$(ARDUINO_CLI) core update-index
	@echo "==> core install esp32:esp32 (this is the few hundred MB)"
	$(ARDUINO_CLI) core install esp32:esp32
	@echo "==> lib install U8g2"
	$(ARDUINO_CLI) lib install U8g2
	@echo
	@echo "setup: done. Next: make flash"

.PHONY: compile
compile: ## Compile the sketch
	$(require_cli)
	$(ARDUINO_CLI) compile --fqbn '$(FQBN)' --build-path $(BUILD_DIR) $(SKETCH)

.PHONY: upload
upload: ## Upload the last build to the board
	$(require_cli)
	$(require_port)
	$(ARDUINO_CLI) upload -p $(PORT) --fqbn '$(FQBN)' --input-dir $(BUILD_DIR) $(SKETCH)

.PHONY: flash
flash: compile upload ## Compile then upload (the one you want)
	@echo
	@echo "flashed to $(PORT). Open the console with: make monitor"

# The parser acts on a newline, so a frame pasted without one does nothing.
.PHONY: monitor
monitor: ## Open the serial console at 115200 (ctrl-c to quit)
	$(require_cli)
	$(require_port)
	@echo "==> $(PORT) at $(BAUD). Paste a frame and press enter. Ctrl-C quits."
	$(ARDUINO_CLI) monitor -p $(PORT) -c baudrate=$(BAUD)

# Delegated rather than duplicated: test/Makefile owns the sanitiser flags and
# the stub include order, and both are load-bearing. See "Host tests".
.PHONY: test
test: ## Run the host test suite under ASan/UBSan (no board needed)
	$(MAKE) -C $(TEST_DIR)

.PHONY: board-details
board-details: ## Print the board's real menu options, to check the FQBN
	$(require_cli)
	$(ARDUINO_CLI) board details -b esp32:esp32:esp32c3

.PHONY: ports
ports: ## List the serial ports arduino-cli can see
	$(require_cli)
	$(ARDUINO_CLI) board list

.PHONY: clean
clean: ## Remove the firmware and host test build directories
	rm -rf $(BUILD_DIR)
	$(MAKE) -C $(TEST_DIR) clean
