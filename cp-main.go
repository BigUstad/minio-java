/*
 * Minio Client (C) 2014, 2015 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"sync"

	"github.com/fatih/color"
	"github.com/minio/cli"
	"github.com/minio/mc/pkg/console"
	"github.com/minio/minio/pkg/probe"
)

// Copy files and folders from many sources to a single destination.
var cpCmd = cli.Command{
	Name:   "cp",
	Usage:  "Copy files and folders from many sources to a single destination.",
	Action: mainCopy,
	CustomHelpTemplate: `NAME:
   mc {{.Name}} - {{.Usage}}

USAGE:
   mc {{.Name}}{{if .Flags}} [ARGS...]{{end}} SOURCE [SOURCE...] TARGET {{if .Description}}

DESCRIPTION:
   {{.Description}}{{end}}{{if .Flags}}

FLAGS:
   {{range .Flags}}{{.}}
   {{end}}{{ end }}

EXAMPLES:
   1. Copy list of objects from local file system to Amazon S3 cloud storage.
      $ mc {{.Name}} Music/*.ogg https://s3.amazonaws.com/jukebox/

   2. Copy a bucket recursively from Minio cloud storage to Amazon S3 cloud storage.
      $ mc {{.Name}} https://play.minio.io:9000/photos/burningman2011... https://s3.amazonaws.com/private-photos/burningman/

   3. Copy multiple local folders recursively to Minio cloud storage.
      $ mc {{.Name}} backup/2014/... backup/2015/... https://play.minio.io:9000/archive/

   4. Copy a bucket recursively from aliased Amazon S3 cloud storage to local filesystem on Windows.
      $ mc {{.Name}} s3:documents/2014/... C:\backup\2014

   5. Copy an object of non english characters to Amazon S3 cloud storage.
      $ mc {{.Name}} 本語 s3:andoria/本語
`,
}

// CopyMessage container for file copy messages
type CopyMessage struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Length int64  `json:"length"`
}

// String string printer for copy message
func (c CopyMessage) String() string {
	if !globalJSONFlag {
		return console.Colorize("Copy", fmt.Sprintf("‘%s’ -> ‘%s’", c.Source, c.Target))
	}
	copyMessageBytes, err := json.Marshal(c)
	fatalIf(probe.NewError(err), "Failed to marshal copy message.")

	return string(copyMessageBytes)
}

// doCopy - Copy a singe file from source to destination
func doCopy(cpURLs copyURLs, bar *barSend, cpQueue <-chan bool, wg *sync.WaitGroup, statusCh chan<- copyURLs) {
	defer wg.Done() // Notify that this copy routine is done.
	defer func() {
		<-cpQueue
	}()

	if cpURLs.Error != nil {
		cpURLs.Error.Trace()
		statusCh <- cpURLs
		return
	}

	if !globalQuietFlag && !globalJSONFlag {
		bar.SetCaption(cpURLs.SourceContent.Name + ": ")
	}

	reader, length, err := getSource(cpURLs.SourceContent.Name)
	if err != nil {
		if !globalQuietFlag || !globalJSONFlag {
			bar.ErrorGet(length)
		}
		cpURLs.Error = err.Trace()
		statusCh <- cpURLs
		return
	}

	var newReader io.ReadCloser
	if globalQuietFlag || globalJSONFlag {
		console.Println(CopyMessage{
			Source: cpURLs.SourceContent.Name,
			Target: cpURLs.TargetContent.Name,
			Length: cpURLs.SourceContent.Size,
		}.String())
		newReader = reader
	} else {
		// set up progress
		newReader = bar.NewProxyReader(reader)
	}
	defer newReader.Close()

	err = putTarget(cpURLs.TargetContent.Name, length, newReader)
	if err != nil {
		if !globalQuietFlag || !globalJSONFlag {
			bar.ErrorPut(length)
		}
		cpURLs.Error = err.Trace()
		statusCh <- cpURLs
		return
	}

	cpURLs.Error = nil // just for safety
	statusCh <- cpURLs
}

// doCopyFake - Perform a fake copy to update the progress bar appropriately.
func doCopyFake(cURLs copyURLs, bar *barSend) {
	if !globalQuietFlag && !globalJSONFlag {
		bar.Progress(cURLs.SourceContent.Size)
	}
}

// doPrepareCopyURLs scans the source URL and prepares a list of objects for copying.
func doPrepareCopyURLs(session *sessionV2, trapCh <-chan bool) {
	// Separate source and target. 'cp' can take only one target,
	// but any number of sources, even the recursive URLs mixed in-between.
	sourceURLs := session.Header.CommandArgs[:len(session.Header.CommandArgs)-1]
	targetURL := session.Header.CommandArgs[len(session.Header.CommandArgs)-1] // Last one is target

	var totalBytes int64
	var totalObjects int

	// Create a session data file to store the processed URLs.
	dataFP := session.NewDataWriter()

	var scanBar scanBarFunc
	if !globalQuietFlag && !globalJSONFlag { // set up progress bar
		scanBar = scanBarFactory("")
	}

	URLsCh := prepareCopyURLs(sourceURLs, targetURL)
	done := false

	for done == false {
		select {
		case cpURLs, ok := <-URLsCh:
			if !ok { // Done with URL prepration
				done = true
				break
			}
			if cpURLs.Error != nil {
				// Print in new line and adjust to top so that we don't print over the ongoing scan bar
				if !globalQuietFlag && !globalJSONFlag {
					console.Printf("%c[2K\n", 27)
					console.Printf("%c[A", 27)
				}
				errorIf(cpURLs.Error.Trace(), "Unable to prepare URLs for copying.")
				break
			}

			jsonData, err := json.Marshal(cpURLs)
			if err != nil {
				session.Close()
				fatalIf(probe.NewError(err), "Unable to prepare URLs for copying. Error in JSON marshaling.")
			}
			fmt.Fprintln(dataFP, string(jsonData))
			if !globalQuietFlag && !globalJSONFlag {
				scanBar(cpURLs.SourceContent.Name)
			}

			totalBytes += cpURLs.SourceContent.Size
			totalObjects++
		case <-trapCh:
			// Print in new line and adjust to top so that we don't print over the ongoing scan bar
			if !globalQuietFlag && !globalJSONFlag {
				console.Printf("%c[2K\n", 27)
				console.Printf("%c[A", 27)
			}
			session.Close() // If we are interrupted during the URL scanning, we drop the session.
			session.Delete()
			os.Exit(0)
		}
	}
	session.Header.TotalBytes = totalBytes
	session.Header.TotalObjects = totalObjects
	session.Save()
}

func doCopyCmdSession(session *sessionV2) {
	trapCh := signalTrap(os.Interrupt, os.Kill)

	if !session.HasData() {
		doPrepareCopyURLs(session, trapCh)
	}

	var bar barSend
	if !globalQuietFlag && !globalJSONFlag { // set up progress bar
		bar = newCpBar()
		bar.Extend(session.Header.TotalBytes)
	}

	// Prepare URL scanner from session data file.
	scanner := bufio.NewScanner(session.NewDataReader())
	// isCopied returns true if an object has been already copied
	// or not. This is useful when we resume from a session.
	isCopied := isCopiedFactory(session.Header.LastCopied)

	wg := new(sync.WaitGroup)
	// Limit number of copy routines based on available CPU resources.
	cpQueue := make(chan bool, int(math.Max(float64(runtime.NumCPU())-1, 1)))
	defer close(cpQueue)

	// Status channel for receiveing copy return status.
	statusCh := make(chan copyURLs)

	// Go routine to monitor doCopy status and signal traps.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case cpURLs, ok := <-statusCh: // Receive status.
				if !ok { // We are done here. Top level function has returned.
					if !globalQuietFlag && !globalJSONFlag {
						bar.Finish()
					}
					return
				}
				if cpURLs.Error == nil {
					session.Header.LastCopied = cpURLs.SourceContent.Name
				} else {
					// Print in new line and adjust to top so that we don't print over the ongoing progress bar
					if !globalQuietFlag && !globalJSONFlag {
						console.Printf("%c[2K\n", 27)
						console.Printf("%c[A", 27)
					}
					errorIf(cpURLs.Error.Trace(), fmt.Sprintf("Failed to copy ‘%s’.", cpURLs.SourceContent.Name))
				}
			case <-trapCh: // Receive interrupt notification.
				// Print in new line and adjust to top so that we don't print over the ongoing progress bar
				if !globalQuietFlag && !globalJSONFlag {
					console.Printf("%c[2K\n", 27)
					console.Printf("%c[A", 27)
				}
				session.Close()
				session.Info()
				os.Exit(0)
			}
		}
	}()

	// Go routine to perform concurrently copying.
	wg.Add(1)
	go func() {
		defer wg.Done()
		copyWg := new(sync.WaitGroup)
		defer close(statusCh)

		for scanner.Scan() {
			var cpURLs copyURLs
			json.Unmarshal([]byte(scanner.Text()), &cpURLs)
			if isCopied(cpURLs.SourceContent.Name) {
				doCopyFake(cpURLs, &bar)
			} else {
				// Wait for other copy routines to
				// complete. We only have limited CPU
				// and network resources.
				cpQueue <- true
				// Account for each copy routines we start.
				copyWg.Add(1)
				// Do copying in background concurrently.
				go doCopy(cpURLs, &bar, cpQueue, copyWg, statusCh)
			}
		}
		copyWg.Wait()
	}()
	wg.Wait()
}

// mainCopy is bound to sub-command
func mainCopy(ctx *cli.Context) {
	checkCopySyntax(ctx)

	console.SetCustomTheme(map[string]*color.Color{
		"Copy": color.New(color.FgGreen, color.Bold),
	})

	session := newSessionV2()

	var e error
	session.Header.CommandType = "cp"
	session.Header.RootPath, e = os.Getwd()
	if e != nil {
		session.Close()
		session.Delete()
		fatalIf(probe.NewError(e), "Unable to get current working folder.")
	}

	// extract URLs.
	var err *probe.Error
	session.Header.CommandArgs, err = args2URLs(ctx.Args())
	if err != nil {
		session.Close()
		session.Delete()
		fatalIf(err.Trace(), "One or more unknown URL types passed.")
	}

	doCopyCmdSession(session)
	session.Close()
	session.Delete()
}