// Copyright Microsoft <wastore@microsoft.com>
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

package ste

import (
	"fmt"
	"github.com/Azure/azure-storage-azcopy/common"
	"log"
	"runtime"
	"strconv"
)

// ConfiguredInt is an integer which may be optionally configured by user through an environment variable
type ConfiguredInt struct {
	Value             int
	IsUserSpecified   bool
	EnvVarName        string
	DefaultSourceDesc string
}

func (i *ConfiguredInt) GetDescription() string {
	if i.IsUserSpecified {
		return fmt.Sprintf("From %s environment variable", i.EnvVarName)
	} else {
		return fmt.Sprintf("From %s. Set %s environment variable to override", i.DefaultSourceDesc, i.EnvVarName)
	}
}

// tryNewConfiguredInt populates a ConfiguredInt from an environment variable, or returns nil if env var is not set
func tryNewConfiguredInt(envVar common.EnvironmentVariable) *ConfiguredInt {
	override := common.GetLifecycleMgr().GetEnvironmentVariable(envVar)
	if override != "" {
		val, err := strconv.ParseInt(override, 10, 64)
		if err != nil {
			log.Fatalf("error parsing the env %s %q failed with error %v",
				envVar.Name, override, err)
		}
		return &ConfiguredInt{int(val), true, envVar.Name, ""}
	}
	return nil
}

// ConcurrencySettings stores the set of related numbers that govern concurrency levels in the STE
type ConcurrencySettings struct {

	// MainPoolSize is the size of the main goroutine pool that transfers the data
	// (i.e. executes chunkfuncs)
	MainPoolSize *ConfiguredInt

	// TransferInitiationPoolSize is the size of the auxiliary goroutine pool that initiates transfers
	// (i.e. creates chunkfuncs)
	TransferInitiationPoolSize *ConfiguredInt

	// MaxIdleConnections is the max number of idle TCP connections to keep open
	MaxIdleConnections int

	// MaxOpenFiles is the max number of file handles that we should have open at any time
	// Currently (July 2019) this is only used for downloads, which is where we wouldn't
	// otherwise have strict control of the number of open files.
	// For uploads, the number of open files is effectively controlled by
	// TransferInitiationPoolSize, since all the file IO (except retries) happens in
	// transfer initiation.
	MaxOpenDownloadFiles int
	// TODO: consider whether we should also use this (renamed to( MaxOpenFiles) for uploads, somehow (see command above). Is there any actual value in that? Maybe only highly handle-constrained Linux environments?
}

const defaultTransferInitiationPoolSize = 64
const concurrentFilesFloor = 32

// NewConcurrencySettings gets concurrency settings by referring to the
// environment variable AZCOPY_CONCURRENCY_VALUE (if set) and to properties of the
// machine where we are running
func NewConcurrencySettings(maxFileAndSocketHandles int) ConcurrencySettings {

	initialMainPoolSize := getMainPoolSize()
	maxMainPoolSize := initialMainPoolSize // one day we may compute a higher value for this, and dynamically grow the pool with this as a cap

	s := ConcurrencySettings{
		MainPoolSize:               initialMainPoolSize,
		TransferInitiationPoolSize: getTransferInitiationPoolSize(),
		MaxOpenDownloadFiles:       getMaxOpenPayloadFiles(maxFileAndSocketHandles, maxMainPoolSize.Value),
	}

	// Set the max idle connections that we allow. If there are any more idle connections
	// than this, they will be closed, and then will result in creation of new connections
	// later if needed. In AzCopy, they almost always will be needed soon after, so better to
	// keep them open.
	// So set this number high so that that will not happen.
	// (Previously, when using Dial instead of DialContext, there was an added benefit of keeping
	// this value high, which was that, without it being high, all the extra dials,
	// to compensate for the closures, were causing a pathological situation
	// where lots and lots of OS threads get created during the creation of new connections
	// (presumably due to some blocking OS call in dial) and the app hits Go's default
	// limit of 10,000 OS threads, and panics and shuts down.  This has been observed
	// on Windows when this value was set to 500 but there were 1000 to 2000 goroutines in the
	// main pool size.  Using DialContext appears to mitigate that issue, so the value
	// we compute here is really just to reduce unneeded make and break of connections)
	s.MaxIdleConnections = maxMainPoolSize.Value

	return s
}

func getMainPoolSize() *ConfiguredInt {
	envVar := common.EEnvironmentVariable.ConcurrencyValue()

	if c := tryNewConfiguredInt(envVar); c != nil {
		return c
	}

	numOfCPUs := runtime.NumCPU()

	var value int

	if numOfCPUs <= 4 {
		// fix the concurrency value for smaller machines
		value = 32
	} else if 16*numOfCPUs > 300 {
		// for machines that are extremely powerful, fix to 300 (previously this was to avoid running out of file descriptors, but we have another solution to that now)
		value = 300
	} else {
		// for moderately powerful machines, compute a reasonable number
		value = 16 * numOfCPUs
	}

	return &ConfiguredInt{value, false, envVar.Name, "number of CPUs"}
}

func getTransferInitiationPoolSize() *ConfiguredInt {
	envVar := common.EEnvironmentVariable.TransferInitiationPoolSize()

	if c := tryNewConfiguredInt(envVar); c != nil {
		return c
	}

	return &ConfiguredInt{defaultTransferInitiationPoolSize, false, envVar.Name, "hard-coded default"}
}

// getMaxOpenFiles finds a number of concurrently-openable files
// such that we'll have enough handles left, after using some as network handles.
// This is important on Unix, where total handles can be constrained.
func getMaxOpenPayloadFiles(maxFileAndSocketHandles int, concurrentConnections int) int {

	// The value we return from this routine here only governs payload files. It does not govern plan
	// files that azcopy opens as part of its own operations.  So we make a reasonable allowance for
	// how many of those may be opened
	const fileHandleAllowanceForPlanFiles = 300 // 300 plan files = 300 * common.NumOfFilesPerDispatchJobPart = 3million in total

	const httpHandleAllowanceForOnGoingEnumeration = 1 // might still be scanning while we are transferring. Make this bigger if we ever do parallel scanning

	// make a conservative estimate of total network and file handles known so far
	estimateOfKnownHandles := int(float32(concurrentConnections)*1.1) +
		fileHandleAllowanceForPlanFiles +
		httpHandleAllowanceForOnGoingEnumeration

	// see what we've got left over for open files
	concurrentFilesLimit := maxFileAndSocketHandles - estimateOfKnownHandles

	// If we get a negative or ridiculously low value, bring it up to some kind of sensible floor
	// (and take our chances of running out of total handles - which is effectively a bet that
	// we were too conservative earlier)
	if concurrentFilesLimit < concurrentFilesFloor {
		concurrentFilesLimit = concurrentFilesFloor
	}
	return concurrentFilesLimit

}