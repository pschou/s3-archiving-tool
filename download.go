package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/remeh/sizedwaitgroup"
)

// DownloadTask represents a file to download.
type DownloadTask struct {
	Size     int64
	Filename string
}

// WorkFile represents a file that has been downloaded.
type WorkFile struct {
	Size     int64
	Filename string

	TempFile string // Temporary file path if the file is large.
	Bytes    []byte // If the file is small, we can keep it in memory.
}

func putMemory(mem []byte) {
	// Function to return memory to the appropriate buffer pool based on size
	mem = mem[:cap(mem)]
	if len(mem) <= 32*1024 {
		bufPool32.Put(mem)
	} else {
		bufPoolLarge.Put(mem)
	}
}

var maxMemObject = int64(EnvInt("MAX_IN_MEM", 96, "Maximum in memory object in kb"))

// Downloader listens for DownloadTask on tasksCh, downloads them, and sends DownloadedFile to doneCh.
func Downloader(ctx context.Context, tasksCh <-chan *DownloadTask, doneCh chan<- *WorkFile) {
	log.Println("Starting downloader...")
	swg := sizedwaitgroup.New(16) // Limit to 16 concurrent downloading parts
	defer close(doneCh)           // Ensure doneCh is closed when the function exits

	for {
		select {
		case <-ctx.Done():
			break
		case task, ok := <-tasksCh:
			if debug {
				log.Printf("Download task: %#v %v\n", task, ok)
			}
			if !ok {
				swg.Wait()
				Println("Closing downloader...")
				return
			}

			parts := 1
			if task.Size > 8*1024*1024 {
				// If file is larger than 8MB, download in parts
				parts = 8
			}
			for i := 0; i < parts; i++ {
				swg.Add() // Add to the sized wait group for each part
			}

			go func(task *DownloadTask, parts int) {
				defer func() {
					for i := 0; i < parts; i++ {
						swg.Done() // Mark the part as done
					}
				}()

				if task.Size == 0 {
					// Empty files just head a header
					doneCh <- &WorkFile{Size: task.Size, Filename: task.Filename}
				} else if task.Size <= maxMemObject*1024 { // If file is less than 32KB, download it in memory.
					// Use a buffer pool to reuse memory for small files
					// bufPool32 is for files <= 32KB, bufPoolLarge is for large files
					// This avoids frequent memory allocations and deallocations.
					var mem []byte
					if task.Size <= 32*1024 {
						mem = bufPool32.Get().([]byte)
					} else {
						mem = bufPoolLarge.Get().([]byte)
					}

					// If the file size is small enough, we can download it directly in memory
					n, err := downloadObjectToBuffer(ctx, srcBucket, task.Filename, mem)
					if err != nil {
						// Log the error and continue to the next file
						fileErrCh <- &ErrorEvent{
							Size:     task.Size,
							Filename: task.Filename,
							Err:      fmt.Errorf("Error downloading object %s to memory: %v", task.Filename, err),
						}
						putMemory(mem)
						return
					}
					// Check if the number of bytes written matches the expected size
					if int64(n) != task.Size {
						fileErrCh <- &ErrorEvent{
							Size:     task.Size,
							Filename: task.Filename,
							Err:      fmt.Errorf("Short write for object %s: expected %d, got %d", task.Filename, task.Size, n),
						}
						putMemory(mem)
						return
					}
					// Successfully downloaded the file to memory
					// Send the downloaded file to doneCh
					doneCh <- &WorkFile{Size: task.Size, Filename: task.Filename,
						Bytes: mem[:n]} // Use the buffer directly as Filebytes
				} else {
					tempFilePath, err := downloadObjectInParts(ctx, srcBucket, task.Filename, task.Size, parts)
					if err != nil {
						// Log the error and continue to the next file
						fileErrCh <- &ErrorEvent{
							Size:     task.Size,
							Filename: task.Filename,
							Err:      fmt.Errorf("Error downloading object %s to temporary file: %v", task.Filename, err),
						}
						return
					}
					// Successfully downloaded the file to a temporary file
					// Send the downloaded file to doneCh
					doneCh <- &WorkFile{Size: task.Size, Filename: task.Filename, TempFile: tempFilePath}
				}
				atomic.AddInt64(&DownloadedFiles, 1)
			}(task, parts)
		}
	}
}
