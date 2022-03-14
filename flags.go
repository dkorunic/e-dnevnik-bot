// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package main

import (
	"time"

	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/pborman/getopt/v2"
	"github.com/sirupsen/logrus"
)

const (
	DefaultConfFile     = ".e-dnevnik.toml" // default configuration filename
	DefaultTickInterval = "1h"              // default (and minimal permitted value) is 1 tick per 1h
)

var (
	debug, daemon                        *bool
	confFile, dbFile, tickIntervalString *string
	tickInterval                         time.Duration
)

// init initializes flags configuration.
func init() {
	debug = getopt.BoolLong("verbose", 'v', "enable verbose/debug log level")
	daemon = getopt.BoolLong("daemon", 'd', "enable daemon mode (running as a service)")
	confFile = getopt.StringLong("conffile", 'f', DefaultConfFile, "configuration file (in TOML)")
	dbFile = getopt.StringLong("database", 'b', db.DefaultDBPath, "alert database file")
	tickIntervalString = getopt.StringLong("interval", 'i', DefaultTickInterval,
		"interval between polls when in daemon mode")
}

// parseFlags parses input arguments and flags.
func parseFlags() {
	getopt.Parse()

	var err error
	tickInterval, err = time.ParseDuration(*tickIntervalString)
	if err != nil {
		logrus.Fatalf("Unable to parse the poll interval %v: %v", tickIntervalString, err)
	}

	if tickInterval < time.Hour {
		logrus.Info("Poll interval is below 1h, so I will default to 1h")
		tickInterval = time.Hour
	}
}
