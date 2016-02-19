# Makefile for the Go Synapse Project
#
# To build the project, you need the gom package from https://github.com/mattn/gom
#

GOMCMD=gom

all: dep-install build test

dep-install:
	$(GOMCMD) install

build:
	$(GOMCMD) build -ldflags "-X main.BuildTime `date -u '+%Y-%m-%d_%H:%M:%S_UTC'` -X main.Version `cat VERSION.txt`-`git rev-parse HEAD`" synapse/synapse
	mv synapse bin/.

test:
	$(GOMCMD) test synapse synapse/output

clean:
	rm -f bin/*
	rm -rf _vendor

install:
	cp bin/synapse /usr/local/bin/synapse
