#
# Copyright (c) 2022 Ben Swartzlander
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

export CGO_ENABLED=0
export GOOS=linux
export GOARCH=amd64
REV=$(shell git describe --long --tags --match='v*' --dirty)

all: build

compile:
	mkdir -p bin
	go build -ldflags '-s -w -X main.version=$(REV)' -o bin/barb *.go

build: compile
	deploy/build.sh quay.io/bswartz/barb:canary

.PHONY: all compile build
