package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"sync/atomic"
	"time"

	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/server"
	"google.golang.org/protobuf/encoding/protojson"
)

type homedDependencies struct {
	read       func(string) ([]byte, error)
	run        func(context.Context, config.Config, server.ListenFunc, func(net.Addr)) error
	runStartup func(context.Context, config.StartupInputs, server.ListenFunc, func(net.Addr)) error
	listen     server.ListenFunc
}

func homedMissingWriter(writer io.Writer) bool {
	if writer == nil {
		return true
	}
	value := reflect.ValueOf(writer)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return value.IsNil()
	default:
		return false
	}
}
func homedDiagnostic(out io.Writer, code obs.LogCode, level obs.LogLevel) {
	if homedMissingWriter(out) {
		return
	}
	event, err := observability.NewEvent(code, level, obs.LogComponent_LOG_COMPONENT_PROCESS, time.Now().UTC(), observability.NewProcessAuthor())
	if err != nil {
		return
	}
	wire, err := protojson.Marshal(event.Wire())
	if err != nil {
		return
	}
	_, _ = out.Write(append(wire, '\n'))
}

// This is the main program's actual runner. It never renders a read, parser,
// provider, panic, address or flag error. The local stream has at most startup,
// ready and terminal diagnostics; the live journal is read through Connect.
func runHomed(ctx context.Context, args []string, out, diagnostics io.Writer, deps homedDependencies) (exitCode int) {
	phase := obs.LogCode_LOG_CODE_PROCESS_CONFIG_READ_FAILED
	var ready atomic.Bool
	defer func() {
		if recover() != nil {
			if phase == obs.LogCode_LOG_CODE_PROCESS_RUNTIME_START_FAILED && ready.Load() {
				phase = obs.LogCode_LOG_CODE_PROCESS_RUNTIME_FAILED
			}
			homedDiagnostic(diagnostics, phase, obs.LogLevel_LOG_LEVEL_ERROR)
			exitCode = 1
		}
	}()
	if ctx == nil || (deps.run == nil) == (deps.runStartup == nil) || deps.run != nil && deps.read == nil || deps.listen == nil || homedMissingWriter(out) || homedMissingWriter(diagnostics) {
		homedDiagnostic(diagnostics, phase, obs.LogLevel_LOG_LEVEL_ERROR)
		return 1
	}
	flags := flag.NewFlagSet("homed", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "config/example.yaml", "initial YAML configuration")
	bootstrapPath := flags.String("bootstrap", "", "existing launch bootstrap YAML")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(out, "Usage: homed -config <path> [-bootstrap <path>]")
			return 0
		}
		homedDiagnostic(diagnostics, obs.LogCode_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED, obs.LogLevel_LOG_LEVEL_ERROR)
		return 2
	}
	if flags.NArg() != 0 {
		homedDiagnostic(diagnostics, obs.LogCode_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED, obs.LogLevel_LOG_LEVEL_ERROR)
		return 2
	}
	var admitted config.Config
	var startup config.StartupInputs
	var err error
	if deps.runStartup != nil {
		phase = obs.LogCode_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED
		choice := config.DefaultBootstrapChoice()
		if *bootstrapPath != "" {
			choice, err = config.ExistingBootstrap(*bootstrapPath)
		}
		if err == nil {
			startup, err = config.LoadStartupPaths(*path, choice, os.LookupEnv)
		}
		if err != nil {
			var pathError *os.PathError
			if errors.As(err, &pathError) {
				phase = obs.LogCode_LOG_CODE_PROCESS_CONFIG_READ_FAILED
			}
			homedDiagnostic(diagnostics, phase, obs.LogLevel_LOG_LEVEL_ERROR)
			return 1
		}
	} else {
		if *bootstrapPath != "" {
			homedDiagnostic(diagnostics, obs.LogCode_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED, obs.LogLevel_LOG_LEVEL_ERROR)
			return 2
		}
		data, readErr := deps.read(*path)
		if readErr != nil {
			homedDiagnostic(diagnostics, phase, obs.LogLevel_LOG_LEVEL_ERROR)
			return 1
		}
		phase = obs.LogCode_LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED
		admitted, err = config.Parse(data)
		clear(data)
		if err != nil {
			homedDiagnostic(diagnostics, phase, obs.LogLevel_LOG_LEVEL_ERROR)
			return 1
		}
	}
	phase = obs.LogCode_LOG_CODE_PROCESS_RUNTIME_START_FAILED
	listenerReady := func(net.Addr) {
		if ready.CompareAndSwap(false, true) {
			homedDiagnostic(out, obs.LogCode_LOG_CODE_PROCESS_READY, obs.LogLevel_LOG_LEVEL_INFO)
		}
	}
	if deps.runStartup != nil {
		err = deps.runStartup(ctx, startup, deps.listen, listenerReady)
	} else {
		err = deps.run(ctx, admitted, deps.listen, listenerReady)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		if ready.Load() {
			phase = obs.LogCode_LOG_CODE_PROCESS_RUNTIME_FAILED
		}
		homedDiagnostic(diagnostics, phase, obs.LogLevel_LOG_LEVEL_ERROR)
		return 1
	}
	homedDiagnostic(out, obs.LogCode_LOG_CODE_PROCESS_STOPPED, obs.LogLevel_LOG_LEVEL_INFO)
	return 0
}
