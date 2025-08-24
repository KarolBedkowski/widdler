#
# Makefile
# Karol Będkowski, 2025-03-25 18:28
#
.PHONY: build
build: # empty.html.bz2
	go build -o widdler -ldflags "-s -w" .

#empty.html.bz2: empty.html
#	bzip2 -9kf empty.html


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
	find . -name '*.go' -type f -exec gofumpt -w {} ';'

.PHONY: run
run:
	go run . -backup.keep_daily 2 -backup.keep_on_write 5 -wikis "`pwd`/wikis/" -log.level debug -backup.interval 5 -backup.mode git-once

.PHONY: run-multi
run-multi:
	go run . -backup.keep_daily 2 -backup.keep_on_write 5 -wikis "`pwd`/wikis/" -log.level debug -backup.interval 5 -backup.mode git-once \
		-htpass `pwd`/.htpasswd -auth basic

# vim:ft=make
#
