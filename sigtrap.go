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
	"os"
	"os/signal"

	"go.uber.org/zap"
)

// TrapSignals尽可能的为当前系统创建正确的signal/interrupt处理。在已经有信号
// 捕获处理的程序中使用这个函数是侵入性的，每个捕获处理都会收到信号通知，导致出
// 现异常，这种情况最好自行实现对应的处理。
// TrapSignals create signal/interrupt handlers as best it can for the
// current OS. This is a rather invasive function to call in a Go program
// that captures signals already, so in that case it would be better to
// implement these handlers yourself.
func TrapSignals() {
	trapSignalsCrossPlatform()
	trapSignalsPosix()
}

// trapSignalsCrossPlatform captures SIGINT or interrupt (depending
// on the OS), which initiates a graceful shutdown. A second SIGINT
// or interrupt will forcefully exit the process immediately.
func trapSignalsCrossPlatform() {
	go func() {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, os.Interrupt)

		// 收到一次os.Interrupt信号（ctrl+c）,开始正常退出，收到多次则直接退出程序
		// 该协程随着主程序的结束而结束
		for i := 0; true; i++ {
			<-shutdown

			if i > 0 {
				Log().Warn("force quit", zap.String("signal", "SIGINT"))
				os.Exit(ExitCodeForceQuit)
			}

			Log().Info("shutting down", zap.String("signal", "SIGINT"))
			go exitProcessFromSignal("SIGINT")
		}
	}()
}

// exitProcessFromSignal exits the process from a system signal.
func exitProcessFromSignal(sigName string) {
	logger := Log().With(zap.String("signal", sigName))
	exitProcess(logger)
}

// Exit codes. Generally, you should NOT
// automatically restart the process if the
// exit code is ExitCodeFailedStartup (1).
const (
	ExitCodeSuccess = iota
	ExitCodeFailedStartup
	ExitCodeForceQuit
	ExitCodeFailedQuit
)
