.PHONY: install uninstall build restart

PLIST_NAME = com.chaspy.agent-exporter
PLIST_SRC  = launchd/$(PLIST_NAME).plist
PLIST_DEST = $(HOME)/Library/LaunchAgents/$(PLIST_NAME).plist

build:
	go build -o $(HOME)/go/bin/agent-exporter ./cmd/agent-exporter

install: build
	cp $(PLIST_SRC) $(PLIST_DEST)
	launchctl load $(PLIST_DEST)
	@echo "agent-exporter started. Logs: /tmp/agent-exporter.log"

uninstall:
	launchctl unload $(PLIST_DEST)
	rm -f $(PLIST_DEST)
	@echo "agent-exporter uninstalled."

restart: uninstall install
