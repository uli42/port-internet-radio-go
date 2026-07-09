package main

import (
	"encoding/json"
	"fmt"
	"log/syslog"
	"os"
	"strings"
)

type LogLevel string

const (
	LogLevelDebug = "DEBUG"
	LogLevelInfo  = "INFO"
	LogLevelWarn  = "WARN"
	LogLevelError = "ERROR"
)

type LogConfig struct {
	StdoutEnabled bool              `json:"stdout_enabled"`
	SyslogEnabled bool              `json:"syslog_enabled"`
	SyslogNetwork string            `json:"syslog_network"`
	SyslogAddr    string            `json:"syslog_addr"`
	LogLevel      string            `json:"log_level"`
	Categories    map[string]string `json:"categories"`
}

var (
	logger        *Logger
	defaultConfig = LogConfig{
		StdoutEnabled: true,
		SyslogEnabled: false,
		SyslogNetwork: "unix",
		SyslogAddr:    "/dev/log",
		LogLevel:      LogLevelInfo,
		Categories:    make(map[string]string),
	}
)

type Logger struct {
	config     LogConfig
	level      LogLevel
	categories map[string]LogLevel
	syslogW    *syslog.Writer
}

func InitLogging(configPath string) error {
	cfg := defaultConfig
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			json.Unmarshal(data, &cfg)
		}
	}
	if cfg.Categories == nil {
		cfg.Categories = make(map[string]string)
	}
	if cfg.SyslogNetwork == "" {
		cfg.SyslogNetwork = "unix"
	}
	if cfg.SyslogAddr == "" {
		cfg.SyslogAddr = "/dev/log"
	}

	l := &Logger{
		config:     cfg,
		categories: make(map[string]LogLevel),
	}

	for cat, lvl := range cfg.Categories {
		l.categories[strings.ToLower(cat)] = parseLogLevel(lvl)
	}
	l.categories["all"] = parseLogLevel(cfg.LogLevel)
	l.level = parseLogLevel(cfg.LogLevel)

	if cfg.SyslogEnabled {
		w, err := syslog.Dial(cfg.SyslogNetwork, cfg.SyslogAddr, syslog.LOG_INFO, "goslimserver")
		if err == nil {
			l.syslogW = w
		}
	}

	logger = l
	return nil
}

func parseLogLevel(s string) LogLevel {
	switch strings.ToUpper(s) {
	case LogLevelDebug:
		return LogLevelDebug
	case LogLevelInfo:
		return LogLevelInfo
	case LogLevelWarn:
		return LogLevelWarn
	case LogLevelError:
		return LogLevelError
	}
	return LogLevelInfo
}

func (l *Logger) shouldLog(category, level string) bool {
	catLevel := l.level
	if cl, ok := l.categories[strings.ToLower(category)]; ok {
		catLevel = cl
	}
	return isLevelEnabled(string(catLevel), level)
}

func isLevelEnabled(cfgLevel, msgLevel string) bool {
	order := map[string]int{
		string(LogLevelDebug): 0,
		string(LogLevelInfo):  1,
		string(LogLevelWarn):  2,
		string(LogLevelError): 3,
	}
	return order[msgLevel] >= order[cfgLevel]
}

func (l *Logger) log(category, level, format string, args ...interface{}) {
	if !l.shouldLog(category, level) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	prefix := fmt.Sprintf("[%s] ", category)

	if l.config.StdoutEnabled {
		fmt.Fprintf(os.Stdout, "%s%s\n", prefix, msg)
	}
	if l.syslogW != nil {
		switch level {
		case LogLevelDebug:
			l.syslogW.Debug(prefix + msg)
		case LogLevelInfo:
			l.syslogW.Info(prefix + msg)
		case LogLevelWarn:
			l.syslogW.Warning(prefix + msg)
		case LogLevelError:
			l.syslogW.Err(prefix + msg)
		}
	}
}

func LogDebug(category, format string, args ...interface{}) {
	if logger != nil {
		logger.log(category, LogLevelDebug, format, args...)
	}
}

func LogInfo(category, format string, args ...interface{}) {
	if logger != nil {
		logger.log(category, LogLevelInfo, format, args...)
	}
}

func LogWarn(category, format string, args ...interface{}) {
	if logger != nil {
		logger.log(category, LogLevelWarn, format, args...)
	}
}

func LogError(category, format string, args ...interface{}) {
	if logger != nil {
		logger.log(category, LogLevelError, format, args...)
	}
}
