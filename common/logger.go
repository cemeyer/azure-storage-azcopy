// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package common

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Microsoft/ApplicationInsights-Go/appinsights"
)

type ILogger interface {
	ShouldLog(level pipeline.LogLevel) bool
	Log(level pipeline.LogLevel, msg string)
	Panic(err error)
}

type ILoggerCloser interface {
	ILogger
	CloseLog()
}

type ILoggerResetable interface {
	OpenLog()
	MinimumLogLevel() pipeline.LogLevel
	ILoggerCloser
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

var telemetry appinsights.TelemetryClient

func GetTelemetryClient() appinsights.TelemetryClient {
	if telemetry != nil {
		return telemetry
	} else {
		if key := lcm.GetEnvironmentVariable(EEnvironmentVariable.AppInsightsInstrumentationKey()); AppInsightsLogging && key != "" {
			telemetry = appinsights.NewTelemetryClient(key)
			telemetry.SetIsEnabled(true)
		}

		return telemetry
	}
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

func NewAppLogger(minimumLevelToLog pipeline.LogLevel, logFileFolder string) ILoggerCloser {
	// TODO: Put start date time in file Name
	// TODO: log life time management.
	//appLogFile, err := os.OpenFile(path.Join(logFileFolder, "azcopy.log"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666) // TODO: Make constant for 0666
	//PanicIfErr(err)
	return &appLogger{
		minimumLevelToLog: minimumLevelToLog,
		//file:              appLogFile,
		//logger:            log.New(appLogFile, "", log.LstdFlags|log.LUTC),
	}
}

type appLogger struct {
	// maximum loglevel represents the maximum severity of log messages which can be logged to Job Log file.
	// any message with severity higher than this will be ignored.
	minimumLevelToLog pipeline.LogLevel // The maximum customer-desired log level for this job
	file              *os.File          // The job's log file
	logger            *log.Logger       // The Job's logger
}

func (al *appLogger) ShouldLog(level pipeline.LogLevel) bool {
	if level == pipeline.LogNone {
		return false
	}
	return level <= al.minimumLevelToLog
}

func (al *appLogger) CloseLog() {
	// TODO consider delete completely to get rid of app logger
	//al.logger.Println("Closing Log")
	//err := al.file.Close()
	//PanicIfErr(err)
}

func (al *appLogger) Log(loglevel pipeline.LogLevel, msg string) {
	// TODO consider delete completely to get rid of app logger
	// TODO: if we DON'T delete, use azCopyLogSanitizer
	//if al.ShouldLog(loglevel) {
	//	al.logger.Println(msg)
	//}
}

func (al *appLogger) Panic(err error) {
	// TODO consider delete completely to get rid of app logger
	//al.logger.Panic(err)
}

////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

// TODO: Maybe find a better way to inform common of app insights logging being enabled?
// Currently this just gets set via the root cmd on the FE.
// I feel like that causes a slight maintainability issue because it doesn't head down the same pipe as all the other options
// but at the same time, I almost prefer this so we don't bloat the plan file, and a user can disable it during a job re-run if needbe.
var AppInsightsLogging = false

type jobLogger struct {
	// maximum loglevel represents the maximum severity of log messages which can be logged to Job Log file.
	// any message with severity higher than this will be ignored.
	jobID             JobID
	minimumLevelToLog pipeline.LogLevel // The maximum customer-desired log level for this job
	file              *os.File          // The job's log file
	logFileFolder     string            // The log file's parent folder, needed for opening the file at the right place
	logger            *log.Logger       // The Job's logger
	appLogger         ILogger
	sanitizer         pipeline.LogSanitizer
}

var LogLevelStrings = map[pipeline.LogLevel]string{
	pipeline.LogFatal:   "FATAL",
	pipeline.LogPanic:   "PANIC",
	pipeline.LogError:   "ERROR",
	pipeline.LogWarning: "WARNING",
	pipeline.LogInfo:    "INFO",
	pipeline.LogDebug:   "DEBUG",
}

func NewJobLogger(jobID JobID, minimumLevelToLog LogLevel, appLogger ILogger, logFileFolder string) ILoggerResetable {
	if appLogger == nil {
		panic("You must pass a appLogger when creating a JobLogger")
	}

	// Initialize telemetry
	GetTelemetryClient()
	if telemetry != nil {
		telemetry.Context().Tags.Session().SetId(jobID.String())
	}

	return &jobLogger{
		jobID:             jobID,
		appLogger:         appLogger, // Panics are recorded in the job log AND in the app log
		minimumLevelToLog: minimumLevelToLog.ToPipelineLogLevel(),
		logFileFolder:     logFileFolder,
		sanitizer:         NewAzCopyLogSanitizer(),
	}
}

func (jl *jobLogger) OpenLog() {
	if jl.minimumLevelToLog == pipeline.LogNone {
		return
	}

	file, err := os.OpenFile(path.Join(jl.logFileFolder, jl.jobID.String()+".log"),
		os.O_RDWR|os.O_CREATE|os.O_APPEND, DEFAULT_FILE_PERM)
	PanicIfErr(err)

	jl.file = file
	jl.logger = log.New(jl.file, "", log.LstdFlags|log.LUTC)
	// Log the Azcopy Version
	jl.logger.Println("AzcopyVersion ", AzcopyVersion)
	jl.appInsightsLog(pipeline.LogInfo, "AzcopyVersion ", AzcopyVersion)
	// Log the OS Environment and OS Architecture
	jl.logger.Println("OS-Environment ", runtime.GOOS)
	jl.appInsightsLog(pipeline.LogInfo, "OS-Environment ", runtime.GOOS)
	jl.logger.Println("OS-Architecture ", runtime.GOARCH)
	jl.appInsightsLog(pipeline.LogInfo, "OS-Architecture ", runtime.GOARCH)
}

func (jl *jobLogger) appInsightsLog(logLevel pipeline.LogLevel, v ...interface{}) {
	if telemetry != nil && jl.ShouldLog(logLevel) {
		if logLevel != pipeline.LogError && logLevel != pipeline.LogPanic {
			event := appinsights.NewEventTelemetry("log")
			event.Properties["message"] = fmt.Sprint(v...)
			event.Properties["level"] = LogLevelStrings[logLevel]
			event.Name = "AzCopy Log Event"
			event.Timestamp = time.Now()
			telemetry.Track(event)
		} else {
			exTel := appinsights.NewExceptionTelemetry(errors.New(fmt.Sprint(v...)))
			exTel.Properties["level"] = LogLevelStrings[logLevel]
			telemetry.Track(exTel)
		}
	}
}

func (jl *jobLogger) MinimumLogLevel() pipeline.LogLevel {
	return jl.minimumLevelToLog
}

func (jl *jobLogger) ShouldLog(level pipeline.LogLevel) bool {
	if level == pipeline.LogNone {
		return false
	}
	return level <= jl.minimumLevelToLog
}

func (jl *jobLogger) CloseLog() {
	jl.logger.Println("Closing Log")
	err := jl.file.Close()
	if telemetry != nil {
		telemetry.Channel().Flush()

		select {
		case <-telemetry.Channel().Close(10 * time.Second):
			// Ten second timeout for retries.

			// If we got here, then all telemetry was submitted
			// successfully, and we can proceed to exiting.
		case <-time.After(30 * time.Second):
			// Thirty second absolute timeout.  This covers any
			// previous telemetry submission that may not have
			// completed before Close was called.

			// There are a number of reasons we could have
			// reached here.  We gave it a go, but telemetry
			// submission failed somewhere.  Perhaps old events
			// were still retrying, or perhaps we're throttled.
			// Either way, we don't want to wait around for it
			// to complete, so let's just exit.
		}
	}
	PanicIfErr(err)
}

func (jl jobLogger) Log(loglevel pipeline.LogLevel, msg string) {
	// If the logger for Job is not initialized i.e file is not open
	// or logger instance is not initialized, then initialize it

	// ensure all secrets are redacted
	msg = jl.sanitizer.SanitizeLogMessage(msg)

	// Go, and therefore the sdk, defaults to \n for line endings, so if the platform has a different line ending,
	// we should replace them to ensure readability on the given platform.
	if lineEnding != "\n" {
		msg = strings.Replace(msg, "\n", lineEnding, -1)
	}
	if jl.ShouldLog(loglevel) {
		jl.logger.Println(msg)
		jl.appInsightsLog(loglevel, msg)
	}
}

func (jl jobLogger) Panic(err error) {
	jl.logger.Println(err) // We do NOT panic here as the app would terminate; we just log it
	jl.appInsightsLog(pipeline.LogPanic, err)
	jl.appLogger.Panic(err) // We panic here that it logs and the app terminates
	// We should never reach this line of code!
}

const TryEquals string = "Try=" // TODO: refactor so that this can be used by the retry policies too?  So that when you search the logs for Try= you are guaranteed to find both types of retry (i.e. request send retries, and body read retries)

func NewReadLogFunc(logger ILogger, fullUrl *url.URL) func(int, error, int64, int64, bool) {
	redactedUrl := URLStringExtension(fullUrl.String()).RedactSecretQueryParamForLogging()

	return func(failureCount int, err error, offset int64, count int64, willRetry bool) {
		retryMessage := "Will retry"
		if !willRetry {
			retryMessage = "Will NOT retry"
		}
		logger.Log(pipeline.LogInfo, fmt.Sprintf(
			"Error reading body of reply. Next try (if any) will be %s%d. %s. Error: %s. Offset: %d  Count: %d URL: %s",
			TryEquals, // so that retry wording for body-read retries is similar to that for URL-hitting retries

			// We log the number of the NEXT try, not the failure just done, so that users searching the log for "Try=2"
			// will find ALL retries, both the request send retries (which are logged as try 2 when they are made) and
			// body read retries (for which only the failure is logged - so if we did the actual failure number, there would be
			// not Try=2 in the logs if the retries work).
			failureCount+1,

			retryMessage,
			err,
			offset,
			count,
			redactedUrl))
	}
}
