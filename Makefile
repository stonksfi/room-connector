.PHONY: build test update-idl install validate-plugin npm-pack npm-publish
IDL_PATH=$(shell pwd)/../http_idl
IDL=$(IDL_PATH)/room/room.proto

# The identity `roomd install` and the npm release sign roomd and roomd-screen
# with, so macOS keeps their screen permissions across rebuilds: the first code
# signing identity in this Mac's keychain unless ROOM_CODESIGN_IDENTITY names
# one. Set it empty to sign ad hoc (the permissions then reset on each install).
ifeq ($(origin ROOM_CODESIGN_IDENTITY),undefined)
ROOM_CODESIGN_IDENTITY := $(shell security find-identity -p codesigning -v 2>/dev/null | awk -F'"' 'NF > 2 { print $$2; exit }')
endif
export ROOM_CODESIGN_IDENTITY

# Build roomd and its screen helper, roomd-screen (cgo; macOS), into ./bin.
build:
	go build -o bin/roomd ./cmd/roomd
	go build -o bin/roomd-screen ./cmd/roomd-screen

# Run the offline test suite (and the npm shim's).
test:
	go test ./...
	node --test npm/roomd/test/

# Regenerate the room-be API models from the shared IDL (http_idl/room).
update-idl:
	hz model -proto_path $(IDL_PATH) -idl $(IDL) -module github.com/stonksfi/room-connector --model_dir internal/model

# Build, then install the daemon (launchd) and the Claude Code plugin.
install: build
	./bin/roomd install

# Check the Claude Code plugin the way Claude Code loads it.
validate-plugin:
	claude plugin validate plugins/claude-code

# Build the npm packages (npm/) for both Mac architectures and pack them into
# dist/ to inspect or install locally: make npm-pack V=0.1.0
npm-pack:
	scripts/npm-release.sh $(V)

# Build and publish them to npm (needs `npm login`): make npm-publish V=0.1.0
npm-publish:
	scripts/npm-release.sh $(V) --publish
