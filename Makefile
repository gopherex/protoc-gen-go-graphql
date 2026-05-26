CURDIR := $(shell pwd)
EXAMPLE_DIR := $(CURDIR)/example
EXAMPLE_OUT := $(EXAMPLE_DIR)/gen
OPTS_DIR := $(CURDIR)/graphqlopt
# Directory holding the well-known-type .proto files (google/protobuf/*.proto).
# Override if your protoc bundles them elsewhere: make WKT_INC=/usr/local/include ...
WKT_INC ?= /usr/include

.PHONY: build
build:
	go build -o $(CURDIR)/bin/protoc-gen-go-graphql ./
	go build -o $(CURDIR)/bin/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	go build -o $(CURDIR)/bin/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc

.PHONY: gen-opts
gen-opts: build
	protoc -I $(CURDIR) -I $(WKT_INC) \
		--plugin=protoc-gen-go=$(CURDIR)/bin/protoc-gen-go \
		--go_out=$(CURDIR) --go_opt=paths=source_relative \
		$(OPTS_DIR)/graphql.proto

.PHONY: gen-test
gen-test: build
	rm -rf $(EXAMPLE_OUT) && mkdir -p $(EXAMPLE_OUT)
	protoc -I $(EXAMPLE_DIR) -I $(CURDIR) -I $(WKT_INC) \
		--plugin=protoc-gen-go=$(CURDIR)/bin/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=$(CURDIR)/bin/protoc-gen-go-grpc \
		--plugin=protoc-gen-go-graphql=$(CURDIR)/bin/protoc-gen-go-graphql \
		--go_out=$(EXAMPLE_OUT) --go_opt=paths=source_relative \
		--go-grpc_out=$(EXAMPLE_OUT) --go-grpc_opt=paths=source_relative \
		--go-graphql_out=$(EXAMPLE_OUT) --go-graphql_opt=paths=source_relative \
		$(EXAMPLE_DIR)/golden.proto
	cd $(EXAMPLE_OUT) && go generate ./...

.PHONY: run-test
run-test:
	go clean -testcache && go test ./...
