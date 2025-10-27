// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelconftelemetry // import "go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"

import (
	"context"
	"strings"

	otelconf "go.opentelemetry.io/contrib/otelconf/v0.3.0"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/internal/telemetry/componentattribute"
	"go.opentelemetry.io/collector/service/telemetry"
)

// createLogger creates a Logger and a LoggerProvider from Config.
func createLogger(
	ctx context.Context,
	set telemetry.LoggerSettings,
	componentConfig component.Config,
) (*zap.Logger, component.ShutdownFunc, error) {
	cfg := componentConfig.(*Config)
	res := newResource(set.Settings, cfg)

	// Copied from NewProductionConfig.
	ec := zap.NewProductionEncoderConfig()
	ec.EncodeTime = zapcore.ISO8601TimeEncoder

	// Separate output paths if rotation is enabled
	outputPaths := cfg.Logs.OutputPaths
	errorOutputPaths := cfg.Logs.ErrorOutputPaths
	var filePaths, errorFilePaths []string

	if cfg.Logs.Rotation != nil {
		// Separate console and file paths for rotation
		outputPaths, filePaths = separateOutputPaths(cfg.Logs.OutputPaths)
		errorOutputPaths, errorFilePaths = separateOutputPaths(cfg.Logs.ErrorOutputPaths)

		// If no console paths remain, add stderr as default
		if len(outputPaths) == 0 {
			outputPaths = []string{"stderr"}
		}
		if len(errorOutputPaths) == 0 {
			errorOutputPaths = []string{"stderr"}
		}
	}

	zapCfg := &zap.Config{
		Level:             zap.NewAtomicLevelAt(cfg.Logs.Level),
		Development:       cfg.Logs.Development,
		Encoding:          cfg.Logs.Encoding,
		EncoderConfig:     ec,
		OutputPaths:       outputPaths,
		ErrorOutputPaths:  errorOutputPaths,
		DisableCaller:     cfg.Logs.DisableCaller,
		DisableStacktrace: cfg.Logs.DisableStacktrace,
		InitialFields:     cfg.Logs.InitialFields,
	}

	if zapCfg.Encoding == "console" {
		// Human-readable timestamps for console format of logs.
		zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}

	logger, err := zapCfg.Build(set.ZapOptions...)
	if err != nil {
		return nil, nil, err
	}

	// Add log rotation for file outputs if configured
	if cfg.Logs.Rotation != nil && (len(filePaths) > 0 || len(errorFilePaths) > 0) {
		logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			cores := []zapcore.Core{c} // Start with the existing core (console outputs)

			// Create encoder based on configuration
			var encoder zapcore.Encoder
			if zapCfg.Encoding == "json" {
				encoder = zapcore.NewJSONEncoder(zapCfg.EncoderConfig)
			} else {
				encoder = zapcore.NewConsoleEncoder(zapCfg.EncoderConfig)
			}

			// Add rotating file cores for regular output paths
			for _, path := range filePaths {
				w := zapcore.AddSync(&lumberjack.Logger{
					Filename:   path,
					MaxSize:    cfg.Logs.Rotation.MaxSizeMB,
					MaxBackups: cfg.Logs.Rotation.MaxBackups,
					MaxAge:     cfg.Logs.Rotation.MaxAgeDays,
					Compress:   cfg.Logs.Rotation.Compress,
				})

				fileCore := zapcore.NewCore(
					encoder,
					w,
					zapCfg.Level,
				)
				cores = append(cores, fileCore)
			}

			// Add rotating file cores for error output paths
			// Error outputs typically only capture error-level logs
			for _, path := range errorFilePaths {
				w := zapcore.AddSync(&lumberjack.Logger{
					Filename:   path,
					MaxSize:    cfg.Logs.Rotation.MaxSizeMB,
					MaxBackups: cfg.Logs.Rotation.MaxBackups,
					MaxAge:     cfg.Logs.Rotation.MaxAgeDays,
					Compress:   cfg.Logs.Rotation.Compress,
				})

				// Error output paths should only log errors
				errorCore := zapcore.NewCore(
					encoder,
					w,
					zapcore.ErrorLevel,
				)
				cores = append(cores, errorCore)
			}

			return zapcore.NewTee(cores...)
		}))
	}

	// The attributes in res.Attributes(), which are generated in telemetry.go,
	// are added to logs exported through the LoggerProvider instantiated below.
	// To make sure they are also exposed in logs written to stdout, we add
	// them as fields to the Zap core created above using WrapCore. We do NOT
	// add them to the logger using With, because that would apply to all logs,
	// even ones exported through the core that wraps the LoggerProvider,
	// meaning that the attributes would be exported twice.
	if res != nil && len(res.Attributes()) > 0 {
		logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			var fields []zap.Field
			for _, attr := range res.Attributes() {
				fields = append(fields, zap.String(string(attr.Key), attr.Value.Emit()))
			}

			r := zap.Dict("resource", fields...)
			return c.With([]zapcore.Field{r})
		}))
	}

	if cfg.Logs.Sampling != nil && cfg.Logs.Sampling.Enabled {
		logger = logger.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
			return zapcore.NewSamplerWithOptions(
				core,
				cfg.Logs.Sampling.Tick,
				cfg.Logs.Sampling.Initial,
				cfg.Logs.Sampling.Thereafter,
			)
		}))
	}

	sdk, err := newSDK(ctx, res, otelconf.OpenTelemetryConfiguration{
		LoggerProvider: &otelconf.LoggerProvider{
			Processors: cfg.Logs.Processors,
		},
	})
	if err != nil {
		return nil, nil, err
	}

	// Wrap the zap.Logger with componentattribute so scope attributes
	// can be added and removed dynamically, and tee logs to the
	// LoggerProvider.
	loggerProvider := sdk.LoggerProvider()
	logger = logger.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		core = componentattribute.NewConsoleCoreWithAttributes(core, attribute.NewSet())
		core = componentattribute.NewOTelTeeCoreWithAttributes(
			core,
			loggerProvider,
			"go.opentelemetry.io/collector/service",
			attribute.NewSet(),
		)
		return core
	}))

	return logger, sdk.Shutdown, nil
}

// separateOutputPaths separates output paths into console outputs (stdout/stderr)
// and file paths for rotation.
func separateOutputPaths(paths []string) (consolePaths []string, filePaths []string) {
	for _, path := range paths {
		// Check if path is stdout, stderr, or starts with file:// scheme pointing to stdout/stderr
		if path == "stdout" || path == "stderr" ||
			strings.HasPrefix(path, "file://stdout") ||
			strings.HasPrefix(path, "file://stderr") {
			consolePaths = append(consolePaths, path)
		} else {
			filePaths = append(filePaths, path)
		}
	}
	return consolePaths, filePaths
}
