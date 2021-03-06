// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package caddy

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/term"
)

func init() {
	RegisterModule(StdoutWriter{})
	RegisterModule(StderrWriter{})
	RegisterModule(DiscardWriter{})
}

// Logging帮助在caddy中记录日志。默认的log名是default，可以自行定制。也可以定义新的log
// 默认INFO及以上等级的日志以可读的格式写入标准错误（如果输出是交互终端使用"console"encoder格式化，
// 其他的使用）"json" encoder
// 所有定义好的日志器接收所有日志条目，但是可以通过日志等级和模块名/日志名过滤。
// 日志器的名字和模块名相同，但是可以在日志器名称后面追加字段来作为模块名，以实现具体功能。
// 例如：可以通过日志名"http.handlers"过滤所有http handlers模块的日志，因为所有http
// handlers模块的名字都包含这样的前缀
// Caddy日志（除了sink）都是0内存分配，在内存和CPU占用上性能很高。启用采样可以进一步
// 提高超高负载服务器上的吞吐量。
// 服务启动时，配置文件里日志的相关配置已解码到该结构体的对应字段中

// Logging facilitates logging within Caddy. The default log is
// called "default" and you can customize it. You can also define
// additional logs.
//
// By default, all logs at INFO level and higher are written to
// standard error ("stderr" writer) in a human-readable format
// ("console" encoder if stdout is an interactive terminal, "json"
// encoder otherwise).
//
// All defined logs accept all log entries by default, but you
// can filter by level and module/logger names. A logger's name
// is the same as the module's name, but a module may append to
// logger names for more specificity. For example, you can
// filter logs emitted only by HTTP handlers using the name
// "http.handlers", because all HTTP handler module names have
// that prefix.
//
// Caddy logs (except the sink) are zero-allocation, so they are
// very high-performing in terms of memory and CPU time. Enabling
// sampling can further increase throughput on extremely high-load
// servers.
type Logging struct {
	// Sink（槽）是所有使用go标准库日志输出的非结构化日志的写入地方。这些日志
	// 格式都不是特地为Caddy设计。 因为是全局的、非结构化的，所以Sink缺少高级特性
	// 和自定义功能。

	// Sink is the destination for all unstructured logs emitted
	// from Go's standard library logger. These logs are common
	// in dependencies that are not designed specifically for use
	// in Caddy. Because it is global and unstructured, the sink
	// lacks most advanced features and customizations.
	Sink *StandardLibLog `json:"sink,omitempty"`

	// Logs代表使用到的日志，以任意指定的名字作为key。通过重新定义名字是"default"
	// 的日志来定制化默认日志。也可以定义其他的日志并设置这些日志接收
	// 哪些日志项

	// Logs are your logs, keyed by an arbitrary name of your
	// choosing. The default log can be customized by defining
	// a log called "default". You can further define other logs
	// and filter what kinds of entries they accept.
	Logs map[string]*CustomLog `json:"logs,omitempty"`

	// 保存所有打开的writer的key；所有用来配置日志器writers的key必须都添加到这个
	// 列表里，以便在清理的时候可以关闭他们

	// a list of all keys for open writers; all writers
	// that are opened to provision this logging config
	// must have their keys added to this list so they
	// can be closed when cleaning up
	writerKeys []string
}

// openLogs sets up the config and opens all the configured writers.
// It closes its logs when ctx is canceled, so it should clean up
// after itself.
func (logging *Logging) openLogs(ctx Context) error {
	// make sure to deallocate resources when context is done
	ctx.OnCancel(func() {
		err := logging.closeLogs()
		if err != nil {
			Log().Error("closing logs", zap.Error(err))
		}
	})

	// 根据Sink的配置加载对应的模块并初始化为可用

	// set up the "sink" log first (std lib's default global logger)
	if logging.Sink != nil {
		err := logging.Sink.provision(ctx, logging)
		if err != nil {
			return fmt.Errorf("setting up sink log: %v", err)
		}
	}

	// 初始化默认日志
	// as a special case, set up the default structured Caddy log next
	if err := logging.setupNewDefault(ctx); err != nil {
		return err
	}

	// then set up any other custom logs
	for name, l := range logging.Logs {
		// the default log is already set up
		if name == "default" {
			continue
		}

		err := l.provision(ctx, logging)
		if err != nil {
			return fmt.Errorf("setting up custom log '%s': %v", name, err)
		}

		// Any other logs that use the discard writer can be deleted
		// entirely. This avoids encoding and processing of each
		// log entry that would just be thrown away anyway. Notably,
		// we do not reach this point for the default log, which MUST
		// exist, otherwise core log emissions would panic because
		// they use the Log() function directly which expects a non-nil
		// logger. Even if we keep logs with a discard writer, they
		// have a nop core, and keeping them at all seems unnecessary.
		if _, ok := l.writerOpener.(*DiscardWriter); ok {
			delete(logging.Logs, name)
			continue
		}
	}

	return nil
}

func (logging *Logging) setupNewDefault(ctx Context) error {
	if logging.Logs == nil {
		logging.Logs = make(map[string]*CustomLog)
	}

	// extract the user-defined default log, if any
	newDefault := new(defaultCustomLog)
	if userDefault, ok := logging.Logs["default"]; ok {
		newDefault.CustomLog = userDefault
	} else {
		// if none, make one with our own default settings
		var err error
		newDefault, err = newDefaultProductionLog()
		if err != nil {
			return fmt.Errorf("setting up default Caddy log: %v", err)
		}
		logging.Logs["default"] = newDefault.CustomLog
	}

	// set up this new log
	err := newDefault.CustomLog.provision(ctx, logging)
	if err != nil {
		return fmt.Errorf("setting up default log: %v", err)
	}
	newDefault.logger = zap.New(newDefault.CustomLog.core)

	// redirect the default caddy logs
	defaultLoggerMu.Lock()
	oldDefault := defaultLogger
	defaultLogger = newDefault
	defaultLoggerMu.Unlock()

	// if the new writer is different, indicate it in the logs for convenience
	var newDefaultLogWriterKey, currentDefaultLogWriterKey string
	var newDefaultLogWriterStr, currentDefaultLogWriterStr string
	if newDefault.writerOpener != nil {
		newDefaultLogWriterKey = newDefault.writerOpener.WriterKey()
		newDefaultLogWriterStr = newDefault.writerOpener.String()
	}
	if oldDefault.writerOpener != nil {
		currentDefaultLogWriterKey = oldDefault.writerOpener.WriterKey()
		currentDefaultLogWriterStr = oldDefault.writerOpener.String()
	}
	if newDefaultLogWriterKey != currentDefaultLogWriterKey {
		oldDefault.logger.Info("redirected default logger",
			zap.String("from", currentDefaultLogWriterStr),
			zap.String("to", newDefaultLogWriterStr),
		)
	}

	return nil
}

// closeLogs cleans up resources allocated during openLogs.
// A successful call to openLogs calls this automatically
// when the context is canceled.
func (logging *Logging) closeLogs() error {
	for _, key := range logging.writerKeys {
		_, err := writers.Delete(key)
		if err != nil {
			log.Printf("[ERROR] Closing log writer %v: %v", key, err)
		}
	}
	return nil
}

// Logger returns a logger that is ready for the module to use.
func (logging *Logging) Logger(mod Module) *zap.Logger {
	modID := string(mod.CaddyModule().ID)
	var cores []zapcore.Core

	if logging != nil {
		for _, l := range logging.Logs {
			if l.matchesModule(modID) {
				if len(l.Include) == 0 && len(l.Exclude) == 0 {
					cores = append(cores, l.core)
					continue
				}
				cores = append(cores, &filteringCore{Core: l.core, cl: l})
			}
		}
	}

	multiCore := zapcore.NewTee(cores...)

	return zap.New(multiCore).Named(modID)
}

// openWriter opens a writer using opener, and returns true if
// the writer is new, or false if the writer already exists.
func (logging *Logging) openWriter(opener WriterOpener) (io.WriteCloser, bool, error) {
	key := opener.WriterKey()
	writer, loaded, err := writers.LoadOrNew(key, func() (Destructor, error) {
		w, err := opener.OpenWriter()
		return writerDestructor{w}, err
	})
	if err != nil {
		return nil, false, err
	}
	logging.writerKeys = append(logging.writerKeys, key)
	return writer.(io.WriteCloser), !loaded, nil
}

// WriterOpener是一个打开日志写入器的模块

// WriterOpener is a module that can open a log writer.
// It can return a human-readable string representation
// of itself so that operators can understand where
// the logs are going.
type WriterOpener interface {
	fmt.Stringer

	// WriterKey is a string that uniquely identifies this
	// writer configuration. It is not shown to humans.
	WriterKey() string

	// OpenWriter opens a log for writing. The writer
	// should be safe for concurrent use but need not
	// be synchronous.
	OpenWriter() (io.WriteCloser, error)
}

type writerDestructor struct {
	io.WriteCloser
}

func (wdest writerDestructor) Destruct() error {
	return wdest.Close()
}

// StandardLibLog配置go标准库log包中的默认全局日志
// 有些不是专门为Caddy开发的模块依赖项可能使用标准库日志

// StandardLibLog configures the default Go standard library
// global logger in the log package. This is necessary because
// module dependencies which are not built specifically for
// Caddy will use the standard logger. This is also known as
// the "sink" logger.
type StandardLibLog struct {
	// The module that writes out log entries for the sink.
	WriterRaw json.RawMessage `json:"writer,omitempty" caddy:"namespace=caddy.logging.writers inline_key=output"`

	writer io.WriteCloser
}

func (sll *StandardLibLog) provision(ctx Context, logging *Logging) error {
	if sll.WriterRaw != nil {
		mod, err := ctx.LoadModule(sll, "WriterRaw")
		if err != nil {
			return fmt.Errorf("loading sink log writer module: %v", err)
		}
		wo := mod.(WriterOpener)

		var isNew bool
		sll.writer, isNew, err = logging.openWriter(wo)
		if err != nil {
			return fmt.Errorf("opening sink log writer %#v: %v", mod, err)
		}

		if isNew {
			log.Printf("[INFO] Redirecting sink to: %s", wo)
			log.SetOutput(sll.writer)
			log.Printf("[INFO] Redirected sink to here (%s)", wo)
		}
	}

	return nil
}

// CustomLog代表自定义logger的配置信息。
//

// CustomLog represents a custom logger configuration.
//
// By default, a log will emit all log entries. Some entries
// will be skipped if sampling is enabled. Further, the Include
// and Exclude parameters define which loggers (by name) are
// allowed or rejected from emitting in this log. If both Include
// and Exclude are populated, their values must be mutually
// exclusive, and longer namespaces have priority. If neither
// are populated, all logs are emitted.
type CustomLog struct {
	// The writer defines where log entries are emitted.
	WriterRaw json.RawMessage `json:"writer,omitempty" caddy:"namespace=caddy.logging.writers inline_key=output"`

	// The encoder is how the log entries are formatted or encoded.
	EncoderRaw json.RawMessage `json:"encoder,omitempty" caddy:"namespace=caddy.logging.encoders inline_key=format"`

	// Level is the minimum level to emit, and is inclusive.
	// Possible levels: DEBUG, INFO, WARN, ERROR, PANIC, and FATAL
	Level string `json:"level,omitempty"`

	// Sampling用来配置日志的采样。启用后，只有部分日志会被输出。可以显著提高极高压力的服务性能。

	// Sampling configures log entry sampling. If enabled,
	// only some log entries will be emitted. This is useful
	// for improving performance on extremely high-pressure
	// servers.
	Sampling *LogSampling `json:"sampling,omitempty"`

	// Include defines the names of loggers to emit in this
	// log. For example, to include only logs emitted by the
	// admin API, you would include "admin.api".
	Include []string `json:"include,omitempty"`

	// Exclude defines the names of loggers that should be
	// skipped by this log. For example, to exclude only
	// HTTP access logs, you would exclude "http.log.access".
	Exclude []string `json:"exclude,omitempty"`

	writerOpener WriterOpener
	writer       io.WriteCloser
	encoder      zapcore.Encoder
	levelEnabler zapcore.LevelEnabler
	core         zapcore.Core
}

func (cl *CustomLog) provision(ctx Context, logging *Logging) error {
	// Replace placeholder for log level
	repl := NewReplacer()
	level, err := repl.ReplaceOrErr(cl.Level, true, true)
	if err != nil {
		return fmt.Errorf("invalid log level: %v", err)
	}
	level = strings.ToLower(level)

	// set up the log level
	switch level {
	case "debug":
		cl.levelEnabler = zapcore.DebugLevel
	case "", "info":
		cl.levelEnabler = zapcore.InfoLevel
	case "warn":
		cl.levelEnabler = zapcore.WarnLevel
	case "error":
		cl.levelEnabler = zapcore.ErrorLevel
	case "panic":
		cl.levelEnabler = zapcore.PanicLevel
	case "fatal":
		cl.levelEnabler = zapcore.FatalLevel
	default:
		return fmt.Errorf("unrecognized log level: %s", cl.Level)
	}

	// If both Include and Exclude lists are populated, then each item must
	// be a superspace or subspace of an item in the other list, because
	// populating both lists means that any given item is either a rule
	// or an exception to another rule. But if the item is not a super-
	// or sub-space of any item in the other list, it is neither a rule
	// nor an exception, and is a contradiction. Ensure, too, that the
	// sets do not intersect, which is also a contradiction.
	if len(cl.Include) > 0 && len(cl.Exclude) > 0 {
		// prevent intersections
		for _, allow := range cl.Include {
			for _, deny := range cl.Exclude {
				if allow == deny {
					return fmt.Errorf("include and exclude must not intersect, but found %s in both lists", allow)
				}
			}
		}

		// ensure namespaces are nested
	outer:
		for _, allow := range cl.Include {
			for _, deny := range cl.Exclude {
				if strings.HasPrefix(allow+".", deny+".") ||
					strings.HasPrefix(deny+".", allow+".") {
					continue outer
				}
			}
			return fmt.Errorf("when both include and exclude are populated, each element must be a superspace or subspace of one in the other list; check '%s' in include", allow)
		}
	}

	if cl.WriterRaw != nil {
		mod, err := ctx.LoadModule(cl, "WriterRaw")
		if err != nil {
			return fmt.Errorf("loading log writer module: %v", err)
		}
		cl.writerOpener = mod.(WriterOpener)
	}
	if cl.writerOpener == nil {
		cl.writerOpener = StderrWriter{}
	}

	cl.writer, _, err = logging.openWriter(cl.writerOpener)
	if err != nil {
		return fmt.Errorf("opening log writer using %#v: %v", cl.writerOpener, err)
	}

	if cl.EncoderRaw != nil {
		mod, err := ctx.LoadModule(cl, "EncoderRaw")
		if err != nil {
			return fmt.Errorf("loading log encoder module: %v", err)
		}
		cl.encoder = mod.(zapcore.Encoder)
	}
	if cl.encoder == nil {
		// only allow colorized output if this log is going to stdout or stderr
		var colorize bool
		switch cl.writerOpener.(type) {
		case StdoutWriter, StderrWriter,
			*StdoutWriter, *StderrWriter:
			colorize = true
		}
		cl.encoder = newDefaultProductionLogEncoder(colorize)
	}

	cl.buildCore()

	return nil
}

func (cl *CustomLog) buildCore() {
	// logs which only discard their output don't need
	// to perform encoding or any other processing steps
	// at all, so just shorcut to a nop core instead
	if _, ok := cl.writerOpener.(*DiscardWriter); ok {
		cl.core = zapcore.NewNopCore()
		return
	}
	c := zapcore.NewCore(
		cl.encoder,
		zapcore.AddSync(cl.writer),
		cl.levelEnabler,
	)
	if cl.Sampling != nil {
		if cl.Sampling.Interval == 0 {
			cl.Sampling.Interval = 1 * time.Second
		}
		if cl.Sampling.First == 0 {
			cl.Sampling.First = 100
		}
		if cl.Sampling.Thereafter == 0 {
			cl.Sampling.Thereafter = 100
		}
		c = zapcore.NewSamplerWithOptions(c, cl.Sampling.Interval,
			cl.Sampling.First, cl.Sampling.Thereafter)
	}
	cl.core = c
}

func (cl *CustomLog) matchesModule(moduleID string) bool {
	return cl.loggerAllowed(moduleID, true)
}

// 如果允许name记录日志，loggerAllowed 返回true。
// 如果name是一个模块的名字或者想要知道是否允许该模块记录日志，isModule参数为true。
// loggerAllowed returns true if name is allowed to emit
// to cl. isModule should be true if name is the name of
// a module and you want to see if ANY of that module's
// logs would be permitted.
func (cl *CustomLog) loggerAllowed(name string, isModule bool) bool {
	// accept all loggers by default
	if len(cl.Include) == 0 && len(cl.Exclude) == 0 {
		return true
	}

	// append a dot so that partial names don't match
	// (i.e. we don't want "foo.b" to match "foo.bar"); we
	// will also have to append a dot when we do HasPrefix
	// below to compensate for when when namespaces are equal
	if name != "" && name != "*" && name != "." {
		name += "."
	}

	var longestAccept, longestReject int

	if len(cl.Include) > 0 {
		for _, namespace := range cl.Include {
			var hasPrefix bool
			if isModule {
				hasPrefix = strings.HasPrefix(namespace+".", name)
			} else {
				hasPrefix = strings.HasPrefix(name, namespace+".")
			}
			if hasPrefix && len(namespace) > longestAccept {
				longestAccept = len(namespace)
			}
		}
		// the include list was populated, meaning that
		// a match in this list is absolutely required
		// if we are to accept the entry
		if longestAccept == 0 {
			return false
		}
	}

	if len(cl.Exclude) > 0 {
		for _, namespace := range cl.Exclude {
			// * == all logs emitted by modules
			// . == all logs emitted by core
			if (namespace == "*" && name != ".") ||
				(namespace == "." && name == ".") {
				return false
			}
			if strings.HasPrefix(name, namespace+".") &&
				len(namespace) > longestReject {
				longestReject = len(namespace)
			}
		}
		// the reject list is populated, so we have to
		// reject this entry if its match is better
		// than the best from the accept list
		if longestReject > longestAccept {
			return false
		}
	}

	return (longestAccept > longestReject) ||
		(len(cl.Include) == 0 && longestReject == 0)
}

// filteringCore filters log entries based on logger name,
// according to the rules of a CustomLog.
type filteringCore struct {
	zapcore.Core
	cl *CustomLog
}

// With properly wraps With.
func (fc *filteringCore) With(fields []zapcore.Field) zapcore.Core {
	return &filteringCore{
		Core: fc.Core.With(fields),
		cl:   fc.cl,
	}
}

// Check only allows the log entry if its logger name
// is allowed from the include/exclude rules of fc.cl.
func (fc *filteringCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if fc.cl.loggerAllowed(e.LoggerName, false) {
		return fc.Core.Check(e, ce)
	}
	return ce
}

// LogSampling configures log entry sampling.
type LogSampling struct {
	// The window over which to conduct sampling.
	Interval time.Duration `json:"interval,omitempty"`

	// Log this many entries within a given level and
	// message for each interval.
	First int `json:"first,omitempty"`

	// If more entries with the same level and message
	// are seen during the same interval, keep one in
	// this many entries until the end of the interval.
	Thereafter int `json:"thereafter,omitempty"`
}

type (
	// StdoutWriter writes logs to standard out.
	StdoutWriter struct{}

	// StderrWriter writes logs to standard error.
	StderrWriter struct{}

	// DiscardWriter discards all writes.
	DiscardWriter struct{}
)

// CaddyModule returns the Caddy module information.
func (StdoutWriter) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID:  "caddy.logging.writers.stdout",
		New: func() Module { return new(StdoutWriter) },
	}
}

// CaddyModule returns the Caddy module information.
func (StderrWriter) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID:  "caddy.logging.writers.stderr",
		New: func() Module { return new(StderrWriter) },
	}
}

// CaddyModule returns the Caddy module information.
func (DiscardWriter) CaddyModule() ModuleInfo {
	return ModuleInfo{
		ID:  "caddy.logging.writers.discard",
		New: func() Module { return new(DiscardWriter) },
	}
}

func (StdoutWriter) String() string  { return "stdout" }
func (StderrWriter) String() string  { return "stderr" }
func (DiscardWriter) String() string { return "discard" }

// WriterKey returns a unique key representing stdout.
func (StdoutWriter) WriterKey() string { return "std:out" }

// WriterKey returns a unique key representing stderr.
func (StderrWriter) WriterKey() string { return "std:err" }

// WriterKey returns a unique key representing discard.
func (DiscardWriter) WriterKey() string { return "discard" }

// OpenWriter returns os.Stdout that can't be closed.
func (StdoutWriter) OpenWriter() (io.WriteCloser, error) {
	return notClosable{os.Stdout}, nil
}

// OpenWriter returns os.Stderr that can't be closed.
func (StderrWriter) OpenWriter() (io.WriteCloser, error) {
	return notClosable{os.Stderr}, nil
}

// OpenWriter returns ioutil.Discard that can't be closed.
func (DiscardWriter) OpenWriter() (io.WriteCloser, error) {
	return notClosable{ioutil.Discard}, nil
}

// notClosable is an io.WriteCloser that can't be closed.
type notClosable struct{ io.Writer }

func (fc notClosable) Close() error { return nil }

type defaultCustomLog struct {
	*CustomLog
	logger *zap.Logger
}

// newDefaultProductionLog configures a custom log that is
// intended for use by default if no other log is specified
// in a config. It writes to stderr, uses the console encoder,
// and enables INFO-level logs and higher.
func newDefaultProductionLog() (*defaultCustomLog, error) {
	cl := new(CustomLog)
	cl.writerOpener = StderrWriter{}
	var err error
	cl.writer, err = cl.writerOpener.OpenWriter()
	if err != nil {
		return nil, err
	}
	cl.encoder = newDefaultProductionLogEncoder(true)
	cl.levelEnabler = zapcore.InfoLevel

	cl.buildCore()

	return &defaultCustomLog{
		CustomLog: cl,
		logger:    zap.New(cl.core),
	}, nil
}

func newDefaultProductionLogEncoder(colorize bool) zapcore.Encoder {
	encCfg := zap.NewProductionEncoderConfig()
	if term.IsTerminal(int(os.Stdout.Fd())) {
		// if interactive terminal, make output more human-readable by default
		encCfg.EncodeTime = func(ts time.Time, encoder zapcore.PrimitiveArrayEncoder) {
			encoder.AppendString(ts.UTC().Format("2006/01/02 15:04:05.000"))
			//encoder.AppendString(ts.Format("2006/01/02 15:04:05.000"))
		}
		if colorize {
			encCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
		}
		return zapcore.NewConsoleEncoder(encCfg)
	}
	return zapcore.NewJSONEncoder(encCfg)
}

// Log returns the current default logger.
func Log() *zap.Logger {
	defaultLoggerMu.RLock()
	defer defaultLoggerMu.RUnlock()
	return defaultLogger.logger
}

var (
	defaultLogger, _ = newDefaultProductionLog()
	defaultLoggerMu  sync.RWMutex
)

var writers = NewUsagePool()

// Interface guards
var (
	_ io.WriteCloser = (*notClosable)(nil)
	_ WriterOpener   = (*StdoutWriter)(nil)
	_ WriterOpener   = (*StderrWriter)(nil)
)
