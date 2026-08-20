PLUGIN := universal-provider
OUTDIR := bin
GO ?= go
export GOTMPDIR := $(CURDIR)/.gotmp

.PHONY: all build test vet fmt clean
all: test vet build

$(GOTMPDIR) $(OUTDIR):
	mkdir -p $@

fmt:
	$(GO)fmt -w *.go

test: $(GOTMPDIR)
	$(GO) test ./...

vet: $(GOTMPDIR)
	$(GO) vet ./...

build: $(GOTMPDIR) $(OUTDIR)
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -trimpath -o $(OUTDIR)/$(PLUGIN).so .
	rm -f $(OUTDIR)/$(PLUGIN).h

test-abi: build
	test -s $(OUTDIR)/$(PLUGIN).so
	nm -D $(OUTDIR)/$(PLUGIN).so | grep -q ' cliproxy_plugin_init$$'

clean:
	rm -rf $(OUTDIR) $(GOTMPDIR)
