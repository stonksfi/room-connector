.PHONY: build test update-idl install validate-plugin
IDL_PATH=$(shell pwd)/../http_idl
IDL=$(IDL_PATH)/room/room.proto

# Build roomd and its screen helper, roomd-screen (cgo; macOS), into ./bin.
build:
	go build -o bin/roomd ./cmd/roomd
	go build -o bin/roomd-screen ./cmd/roomd-screen

# Run the offline test suite.
test:
	go test ./...

# Regenerate the room-be API models from the shared IDL (http_idl/room).
update-idl:
	hz model -proto_path $(IDL_PATH) -idl $(IDL) -module github.com/stonksfi/room-connector --model_dir internal/model

# Build, then install the daemon (launchd) and the Claude Code plugin. Set
# ROOM_CODESIGN_IDENTITY to a signing identity so macOS keeps the screen
# permissions across reinstalls.
install: build
	./bin/roomd install

# Check the Claude Code plugin the way Claude Code loads it.
validate-plugin:
	claude plugin validate plugins/claude-code
