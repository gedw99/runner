// Copyright 2017 github.com/ucirello
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Command runner is a very ugly and simple structured command executer that
monitor file changes to trigger process restarts.

Create a file name Procfile in the root of the project you want to run, and add
the following content:

	workdir: $GOPATH/src/github.com/example/go-app
	observe: *.go *.js
	ignore: /vendor
	build-server: make server
	web-a: group=web restart=always waitfor=localhost:8888 ./server serve alpha
	web-b: group=web restart=always waitfor=localhost:8888 ./server serve bravo
	db: restart=failure waitfor=localhost:8888 ./server db

Special process types:

- workdir: the working directory. Environment variables are expanded. It follows
the same rules for exec.Command.Dir.

- observe: a space separated list of file patterns to scan for. It uses
filepath.Match internally.

- ignore: a space separated list of ignored directories relative to workdir,
typically vendor directories.

- build*: process type name prefixed by "build" are always executed first and in
order of declaration. On failure, they halt the initialization.

- waitfor (in process type): target hostname and port that the runner will probe
before starting the process type.

- restart (in process type): "always" will restart the process type at every
build; "fail" will restart the process type on failure; "temporary" will start
the service once and not restart it on rebuilds; "loop" will restart the process
when it naturally terminates.

- group (in process type): group of processes that depend on each other. If a
process type fails, it will halt all others in the same group. If the
"restart" paramater is not set to "always" or "fail", the affected process
types will halt and not restart.

- sticky (in build process types): a sticky build is not interrupted when file
changes are detected.

- optional (in process types): does not start this process unless explicit told
so. The process type must be part of a group.
*/
package main // import "cirello.io/runner"

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cirello.io/runner/procfile"
	"cirello.io/runner/runner"
	cli "gopkg.in/urfave/cli.v1"
)

// DefaultProcfile is the file that runner will open by default if no custom
// is given.
const DefaultProcfile = "Procfile"

func main() {
	log.SetFlags(0)

	facet := filepath.Base(os.Args[0])

	switch facet {
	case "waitfor":
		log.SetPrefix("waitfor: ")
		waitfor()
	default:
		log.SetPrefix("runner: ")
		run()
	}

}

func waitfor() {
	app := cli.NewApp()
	app.Name = "waitfor"
	app.Usage = "probes the service discovery before running the given command"
	app.HideVersion = true
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "service-discovery",
			Value: "localhost:64000",
			Usage: "service discovery address",
		},
	}
	app.Action = func(c *cli.Context) error {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-sigint
			log.Println("shutting down")
			cancel()
		}()

		for {
			time.Sleep(250 * time.Millisecond)
			resp, err := http.Get("http://" + c.String("service-discovery"))
			if err != nil {
				continue
			}
			defer resp.Body.Close()
			var procs map[string]string // map of procType to state
			if err := json.NewDecoder(resp.Body).Decode(&procs); err != nil {
				return fmt.Errorf("cannot parse state of service discovery endpoint: %v", err)
			}
			allBuilt := true
			for k, v := range procs {
				if !strings.HasPrefix(k, "BUILD_") || v == "done" {
					continue
				}
				allBuilt = false
				break

			}
			if allBuilt {
				break
			}
		}

		cmd := strings.Join(c.Args(), " ")
		cmdExec := exec.CommandContext(ctx, "sh", "-c", cmd)
		cmdExec.Stdin = os.Stdin
		cmdExec.Stderr = os.Stderr
		cmdExec.Stdout = os.Stdout
		cmdExec.Run()

		return nil
	}
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func run() {
	app := cli.NewApp()
	app.Name = "runner"
	app.Usage = "simple Procfile runner"
	app.HideVersion = true
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "convert",
			Usage: "takes a declared Procfile and prints as JSON to standard output",
		},
		cli.IntFlag{
			Name:  "port",
			Value: 5000,
			Usage: "base IP port used to set $`PORT` for each process type. Should be multiple of 1000.",
		},
		cli.StringFlag{
			Name:  "service-discovery",
			Value: "localhost:64000",
			Usage: "service discovery address",
		},
		cli.StringFlag{
			Name:  "formation",
			Value: "",
			Usage: "formation allows to start more than one instance of a process type, format: `procTypeA=# procTypeB=# ... procTypeN=#`",
		},
		cli.StringFlag{
			Name:  "env",
			Value: ".env",
			Usage: "environment `file` to be loaded for all processes.",
		},
		cli.StringFlag{
			Name:  "skip",
			Value: "",
			Usage: "does not run some of the process types, format: `procTypeA procTypeB procTypeN`",
		},
		cli.StringFlag{
			Name:  "only",
			Value: "",
			Usage: "only runs some of the process types, format: `procTypeA procTypeB procTypeN`",
		},
		cli.StringFlag{
			Name:  "optional",
			Value: "",
			Usage: "forcefully runs some of the process types, format: `procTypeA procTypeB procTypeN`",
		},
	}
	app.Action = mainRunner
	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func mainRunner(c *cli.Context) error {
	origStdout := os.Stdout
	basePort := c.Int("port")
	convertToJSON := c.Bool("convert")
	envFN := c.String("env")
	skipProcs := c.String("skip")
	onlyProcs := c.String("only")
	optionalProcs := c.String("optional")
	discoveryAddr := c.String("service-discovery")

	var (
		filterPatternMu sync.RWMutex
		filterPattern   string
	)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			text := scanner.Text()
			filterPatternMu.Lock()
			filterPattern = text
			filterPatternMu.Unlock()
			if text != "" {
				log.Println("filtering with:", scanner.Text())
			}
		}
		if err := scanner.Err(); err != nil {
			log.Println("reading standard output:", err)
		}
	}()
	go func() {
		r, w, _ := os.Pipe()
		os.Stdout = w
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 2097152), 262144)
		for scanner.Scan() {
			filterPatternMu.RLock()
			p := filterPattern
			filterPatternMu.RUnlock()
			text := scanner.Text()
			if p == "" {
				fmt.Fprintln(origStdout, text)
				continue
			}

			words := strings.Fields(p)
			for _, w := range words {
				if strings.Contains(text, w) {
					fmt.Fprintln(origStdout, text)
					break
				}
			}
		}
		if err := scanner.Err(); err != nil {
			log.Println("reading standard output:", err)
		}
	}()

	fn := DefaultProcfile
	if argFn := flag.Arg(0); argFn != "" {
		fn = argFn
	}

	fd, err := os.Open(fn)
	if err != nil {
		return err
	}

	if basePort < 1 || basePort > 65535 {
		return errors.New("invalid IP port")
	}

	var s runner.Runner

	switch filepath.Ext(fn) {
	case ".json":
		if err := json.NewDecoder(fd).Decode(&s); err != nil {
			return fmt.Errorf("cannot parse spec file (json): %v", err)
		}
	default:
		s, err = procfile.Parse(fd)
		if err != nil {
			return fmt.Errorf("cannot parse spec file (procfile): %v", err)
		}
	}

	if convertToJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "    ")
		if err := enc.Encode(&s); err != nil {
			return fmt.Errorf("cannot encode procfile into JSON: %v", err)
		}
		return nil
	}

	s.WorkDir = os.ExpandEnv(s.WorkDir)
	if s.WorkDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot load current workdir: %v", err)
		}
		s.WorkDir = wd
	}

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sigint
		log.Println("shutting down")
		cancel()
	}()

	s.BasePort = basePort

	if fd, err := os.Open(envFN); err == nil {
		scanner := bufio.NewScanner(fd)
		for scanner.Scan() {
			line := strings.Split(strings.TrimSpace(scanner.Text()), "=")
			if len(line) != 2 {
				continue
			}

			s.BaseEnvironment = append(s.BaseEnvironment, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("error reading environment file (%v): %v", envFN, err)
		}
	}

	if skipProcs != "" {
		s.Processes = filterSkippedProcs(skipProcs, s.Processes)
	} else if onlyProcs != "" {
		s.Processes = filterOnlyProcs(onlyProcs, s.Processes)
	}
	if optionalProcs != "" {
		s.Processes = keepOptionalProcs(optionalProcs, s.Processes)
	} else {
		s.Processes = filterOptionalProcs(s.Processes)
	}
	s.ServiceDiscoveryAddr = discoveryAddr
	if err := s.Start(ctx); err != nil {
		return fmt.Errorf("cannot serve: %v", err)
	}
	return nil
}

func keepOptionalProcs(optionals string, processes []*runner.ProcessType) []*runner.ProcessType {
	optionalProcs, newProcs := strings.Split(optionals, " "), []*runner.ProcessType{}
procTypes:
	for _, procType := range processes {
		if !procType.Optional {
			newProcs = append(newProcs, procType)
			continue
		}
		for _, optional := range optionalProcs {
			if procType.Name == optional {
				fmt.Println("enabling", optional)
				newProcs = append(newProcs, procType)
				continue procTypes
			}
		}
	}
	return newProcs
}

func filterOptionalProcs(processes []*runner.ProcessType) []*runner.ProcessType {
	groups := make(map[string]struct{})
	newProcs := []*runner.ProcessType{}
	for _, procType := range processes {
		if procType.Optional && procType.Group == "" {
			continue
		} else if procType.Optional && procType.Group != "" {
			if _, ok := groups[procType.Group]; ok {
				continue
			}
			groups[procType.Group] = struct{}{}
		}
		newProcs = append(newProcs, procType)
	}
	return newProcs
}

func filterSkippedProcs(skip string, processes []*runner.ProcessType) []*runner.ProcessType {
	skipProcs, newProcs := strings.Split(skip, " "), []*runner.ProcessType{}
procTypes:
	for _, procType := range processes {
		for _, skip := range skipProcs {
			if procType.Name == skip {
				fmt.Println("skipping", skip)
				continue procTypes
			}
		}
		newProcs = append(newProcs, procType)
	}
	return newProcs
}

func filterOnlyProcs(only string, processes []*runner.ProcessType) []*runner.ProcessType {
	onlyProcs, newProcs := strings.Split(only, " "), []*runner.ProcessType{}
procTypes:
	for _, procType := range processes {
		for _, only := range onlyProcs {
			if procType.Name == only {
				newProcs = append(newProcs, procType)
				continue procTypes
			}
		}
	}
	return newProcs
}
