# Build tinysonic and deploy it to a Linux server over ssh.
#
#   make setup  HOST=me@cloud.ademant.de   # once: user, directories, unit, config
#   make deploy HOST=me@cloud.ademant.de   # check, build, upload, restart, health check
#
# The server side lives in deploy/remote.sh. Root steps run through sudo, so
# the ssh user needs sudo rights there. The reverse proxy is not touched; see
# deploy/nginx-subpath.conf and deploy/Caddyfile.

HOST   ?=
# The HTTPS vhost that serves tinysonic under /folksonic (make apache, #161).
APACHE_VHOST ?= /etc/apache2/sites-enabled/000-default-le-ssl.conf
GOOS   ?= linux
GOARCH ?= amd64

# Compiling the SQLite package (go-sqlite3-wasm) with Go's defaults needs well
# over 1 GB of RAM and gets OOM-killed on small servers. One package at a
# time, a single-threaded compiler backend and a lower GC target keep the peak
# near 750 MB. On a bigger machine, build at full speed with: make build GOFLAGS= GOGC=
export GOGC    ?= 20
export GOFLAGS ?= -p=1 -gcflags=all=-c=1

NAME  := tinysonic
DIST  := dist/$(NAME)
STAGE := $(NAME)-deploy

# stage copies files into a fresh ~/$(STAGE) on the server.
stage = ssh $(HOST) 'rm -rf $(STAGE) && mkdir -m 0700 $(STAGE)' && scp $(1) $(HOST):$(STAGE)/

.PHONY: build check setup deploy rollback apache status logs clean require-host

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -o $(DIST) ./cmd/$(NAME)

check:
	go build ./...
	go vet ./...
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi
	go test ./...

setup: require-host
	$(call stage,deploy/remote.sh deploy/$(NAME).service deploy/folksonic.toml)
	ssh -t $(HOST) sh $(STAGE)/remote.sh setup
	@echo "Next: edit /etc/$(NAME)/$(NAME).toml on $(HOST), include deploy/nginx-subpath.conf in the site's server block, then run make deploy HOST=$(HOST)"

deploy: require-host check build
	$(call stage,deploy/remote.sh $(DIST))
	ssh -t $(HOST) sh $(STAGE)/remote.sh deploy

rollback: require-host
	$(call stage,deploy/remote.sh)
	ssh -t $(HOST) sh $(STAGE)/remote.sh rollback

# apache adds the device relay's WebSocket proxy lines to the HTTPS vhost (#161).
# It is idempotent and keeps a backup. Override the vhost with APACHE_VHOST=…
apache: require-host
	$(call stage,deploy/remote.sh deploy/apache-subpath.conf)
	ssh -t $(HOST) sh $(STAGE)/remote.sh apache '$(APACHE_VHOST)'

status: require-host
	ssh -t $(HOST) systemctl --no-pager status $(NAME)

logs: require-host
	ssh -t $(HOST) sudo journalctl -u $(NAME) -n 100 -f

clean:
	rm -rf dist

require-host:
	@test -n "$(HOST)" || { echo "set HOST, for example: make $(MAKECMDGOALS) HOST=me@server" >&2; exit 1; }
