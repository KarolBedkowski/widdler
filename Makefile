#
# Makefile
# Karol Będkowski, 2025-03-25 18:28
#
.PHONY: build
build: generate
	go build -o widdler-ex main.go


.PHONY: build_release
build_release: 
	go generate
	go build -o widdler-ex -ldflags "-s -w" main.go

.PHONY: check
lint:
	golangci-lint run || true
	#--fix
	# go install go.uber.org/nilaway/cmd/nilaway@latest
	nilaway ./... || true
	typos *.go

.PHONY: format
format:
	# find internal cli -type d -exec wsl -fix prom-dbquery_exporter.app/{} ';'
	# find . -name '*.go' -type f -exec gofumpt -w {} ';'
	golangci-lint fmt

.PHONY: run
run: generate
	go run -tags gitint main.go \
		--log.level debug \
		serve \
		--wikis "`pwd`/wikis/" \
		--backup.mode sqlite \
		--backup.interval 5 \
		--backup.policy 2,5,30s \
		--backup.compress


#-backup.mode git \
#--auth basic \

.PHONY: run-multi
run-multi: generate
	go run -tags gitint main.go \
		--log.level debug \
		serve \
 		--htpass `pwd`/.htpasswd --auth basic \
		--wikis "`pwd`/wikis/" \
		--backup.mode sqlite \
		--backup.interval 5 \
		--backup.policy 2,5,30s \
		--backup.compress

.PHONY: clean
clean:
	find . -type f -name '*.qtpl.go' -delete


QTPLS := $(shell find . -type f -name '*.qtpl')
QTPLSC := $(QTPLS:%=%.go)

generate: $(QTPLSC)

%.qtpl.go: %.qtpl
	qtc -file $<

.PHONY: clean
prepare:
	go install github.com/valyala/quicktemplate/qtc
	go mod tidy

.PHONY: update-deps
update-deps:
	go get -u ./...
	go mod tidy

# vim:ft=make
#
