# Makefile - host-compiled checks for the Stoplight firmware.
#
#   make          build and run
#   make run      the same
#   make clean    remove the build directory
#
# The firmware modules are compiled for the HOST against the stubs in stubs/,
# which replace Arduino.h and U8g2lib.h. Nothing here talks to a board. See
# firmware/readme.md, "Host tests".
#
# The sanitisers are not optional. Every buffer in the firmware is fixed size
# and the parser runs on untrusted input, so an off-by-one is the failure mode
# most worth catching, and it is exactly what ASan finds and a passing
# assertion does not.

CXX ?= c++

SRC_DIR   := ../stoplight
STUB_DIR  := stubs
BUILD_DIR := build
BIN       := $(BUILD_DIR)/test_stoplight

# -I stubs comes FIRST so that #include <Arduino.h> and <U8g2lib.h> resolve to
# the stubs rather than to anything installed on the machine.
# -MMD -MP emit a .d file of header dependencies beside each .o.
#
# Without them an object depended only on its .cpp, so editing a HEADER
# rebuilt nothing. Change a member in display.h and test_main.o keeps the old
# object layout while display.o uses the new one: the two then disagree about
# sizeof(Display), and the result is a stack-buffer-overflow reported inside
# the constructor that looks exactly like a real firmware bug. That cost a
# diagnosis during the blank-screen investigation. The compiler already knows
# the true dependencies, so let it record them.
CXXFLAGS := -std=c++17 -g -O1 \
            -I$(STUB_DIR) -I$(SRC_DIR) \
            -Wall -Wextra -Wconversion -Wshadow \
            -fsanitize=address,undefined \
            -fno-omit-frame-pointer \
            -MMD -MP

LDFLAGS := -fsanitize=address,undefined

# Abort on the first undefined-behaviour report rather than logging it and
# carrying on, so UB cannot hide behind a passing exit code.
export UBSAN_OPTIONS := halt_on_error=1:print_stacktrace=1
export ASAN_OPTIONS  := detect_leaks=0

SOURCES := test_main.cpp \
           $(STUB_DIR)/stubs.cpp \
           $(SRC_DIR)/protocol.cpp \
           $(SRC_DIR)/display.cpp \
           $(SRC_DIR)/lamps.cpp \
           $(SRC_DIR)/button.cpp

OBJECTS := $(addprefix $(BUILD_DIR)/,$(notdir $(SOURCES:.cpp=.o)))

VPATH := .:$(STUB_DIR):$(SRC_DIR)

.PHONY: all run clean

all: run

run: $(BIN)
	@echo
	@./$(BIN)

$(BIN): $(OBJECTS)
	$(CXX) $(LDFLAGS) -o $@ $^

$(BUILD_DIR)/%.o: %.cpp | $(BUILD_DIR)
	$(CXX) $(CXXFLAGS) -c -o $@ $<

$(BUILD_DIR):
	@mkdir -p $(BUILD_DIR)

# Pull in the generated header dependencies. The leading - keeps the first
# build, where no .d files exist yet, quiet.
-include $(OBJECTS:.o=.d)

clean:
	@rm -rf $(BUILD_DIR)
