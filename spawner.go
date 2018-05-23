package main

import (
	"fmt"
	"github.com/f4n4t/gopoststuff/simplenntp"
	"os"
	"path/filepath"
	"sync"
	"time"
	"github.com/dustin/go-humanize"
)

type FileData struct {
	path string
	size int64
}

type Totals struct {
	start time.Time
	end   time.Time
	bytes int64
}

func Spawner(filenames []string) {
	var wg sync.WaitGroup
	files := make([]FileData, 0)

	var altnzbpath string
	nzbinfo := make(map[string]NzbFile, 0)
	segs := make(map[string][]NzbSegment, 0)

	var mutex = &sync.Mutex{}

	log.Debug("Spawner started")

	// Walk any directories and collect files
	for _, filename := range filenames {
		err := filepath.Walk(filename, func(path string, fi os.FileInfo, err error) error {
			if !fi.IsDir() && fi.Size() > 0 {
				files = append(files, FileData{path: path, size: fi.Size()})
			}
			return err
		})
		if err != nil {
			log.Fatalf("Spawner walk error: %s", err)
		}
	}

	// Log a message about what we're posting
	var totalBytes int64
	for _, fd := range files {
		totalBytes += int64(fd.size)
	}
	totalMB := float64(totalBytes) / 1024 / 1024
	log.Infof("Found %d file(s) totalling %.1fMiB", len(files), totalMB)

	// Make a channel to stuff TimeDatas into
	tdchan := make(chan *simplenntp.TimeData, 100000)

	// Use specified server
	serverList := make(map[string]*ConfigServer, len(Config.Server))
	if len(*serverFlag) > 0 {
		serverList[*serverFlag] = Config.Server[*serverFlag]
	} else if len(Config.Global.DefaultServer) > 0 {
		serverList[Config.Global.DefaultServer] = Config.Server[Config.Global.DefaultServer]
	} else {
		serverList = Config.Server
	}
	// Iterate over configured servers
	for name, server := range serverList {
		log.Infof("[%s] Starting %d connections", name, server.Connections)

		// Make a channel to stuff Articles into
		achan := make(chan *Article, server.Connections)

		// Make a channel to stuff Totals into
		tchan := make(chan *Totals, server.Connections)

		// Start a goroutine to generate articles
		wg.Add(1)
		go func(c chan *Article, files []FileData) {
			defer wg.Done()

			log.Debugf("[%s] Article generator started", name)

			mc := NewMmapCache()
			for filenum, fd := range files {
				// Open and mmap the file
				md, err := mc.MapFile(fd.path, len(serverList))
				if err != nil {
					log.Fatalf("MapFile error: %s", err)
				}

				// Work out how many parts we need
				parts := fd.size / Config.Global.ArticleSize
				rem := fd.size % Config.Global.ArticleSize
				if rem > 0 {
					parts++
				}

				// Build some articles
				for partnum := int64(0); partnum < parts; partnum++ {
					start := partnum * Config.Global.ArticleSize
					end := min((partnum+1)*Config.Global.ArticleSize, fd.size)
					ad := &ArticleData{
						PartNum:   partnum + 1,
						PartTotal: parts,
						PartSize:  end - start,
						PartBegin: start,
						PartEnd:   end,
						FileNum:   filenum + 1,
						FileTotal: len(files),
						FileSize:  fd.size,
						FileName:  filepath.Base(fd.path),
					}
					var subject string
					if *dirSubjectFlag {
						subject = filepath.Base(filepath.Dir(fd.path))
					} else {
						subject = *subjectFlag
					}
					if len(altnzbpath) == 0 {
						altnzbpath = SafeFileName(subject)
					}
					a := NewArticle(md.data[start:end], ad, subject)
					c <- a
				}

				if md.Decrement() {
					err = mc.CloseFile(fd.path)
					if err != nil {
						log.Fatalf("CloseFile error: %s", err)
					}
					log.Debugf("[%s] Closed file %s", name, fd.path)
				}
			}

			close(c)
		}(achan, files)

		// Start a goroutine for each individual connection
		for i := 0; i < server.Connections; i++ {
			connID := i + 1

			// Increment the WaitGroup counters
			wg.Add(1)
			go func(achan chan *Article, tchan chan *Totals) {
				// Decrement the counter when the goroutine completes
				defer wg.Done()

				// Connect
				log.Debugf("[%s:%02d] Connecting...", name, connID)
				conn, err := simplenntp.Dial(server.Address, server.Port, server.TLS, server.InsecureSSL, tdchan)
				if err != nil {
					log.Fatalf("[%s] Error while connecting: %s", name, err)
				}
				log.Debugf("[%s:%02d] Connected", name, connID)

				// Authenticate if required
				if len(server.Username) > 0 {
					log.Debugf("[%s:%02d] Authenticating...", name, connID)
					err := conn.Authenticate(server.Username, server.Password)
					if err != nil {
						log.Fatalf("[%s:%02d] Error while authenticating: %s", name, connID, err)
					}
					log.Debugf("[%s:%02d] Authenticated", name, connID)
				}

				t := &Totals{start: time.Now()}

				// Begin consuming
				for article := range achan {
					err := conn.Post(article.Body, Config.Global.ChunkSize)
					if err != nil {
						log.Fatalf("[%s:%02d] Post error: %s", name, connID, err)
					} else {
						mutex.Lock()
						nzbinfo[article.FileName] = article.NzbData
						segs[article.FileName] = append(segs[article.FileName], article.Segment)
						mutex.Unlock()
					}
					t.bytes += int64(len(article.Body))
				}

				// Stick our totals struct into the channel
				t.end = time.Now()
				tchan <- t

				// Close the connection
				log.Debugf("[%s:%02d] Closing connection", name, connID)
				err = conn.Quit()
				if err != nil {
					log.Warningf("[%s:%02d] Error while closing connection: %s", name, connID, err)
				}
			}(achan, tchan)
		}
		// Start a goroutine to print some stats when done, sigh
		wg.Add(1)
		go func(tchan chan *Totals) {
			defer wg.Done()

			minStart := time.Now()
			var maxEnd time.Time
			var totalBytes int64

			for i := 0; i < server.Connections; i++ {
				t := <-tchan
				if t.start.Before(minStart) {
					minStart = t.start
				}
				if t.end.After(maxEnd) {
					maxEnd = t.end
				}
				totalBytes += t.bytes
			}

			// Calculate and log the result
			dur := maxEnd.Sub(minStart)
			speed := float64(totalBytes) / dur.Seconds()
			totalMB := float64(totalBytes) / 1024 / 1024

			log.Infof("[%s] Posted %.1fMiB in %s at %s/s", name, totalMB, dur.String(), humanize.Bytes(uint64(speed)))
		}(tchan)
	}

	// Start our weird status goroutine
	statusTicker := time.NewTicker(time.Second * 1)
	go StatusLogger(statusTicker, tdchan)

	// Wait for all connections to complete
	wg.Wait()

	// Generate Nzb
	var nzbpath string
	if len(*nzbFlag) > 0 {
		nzbpath = *nzbFlag
	} else if len(Config.Global.DefaultNzb) > 0 {
		nzbpath = Config.Global.DefaultNzb
	} else {
		nzbpath = fmt.Sprintf("gps-%d_%s.nzb", time.Now().Unix(), altnzbpath)
	}

	if _, err := os.Stat(nzbpath); err == nil {
		log.Warningf("Nzbfile already exists: %s", nzbpath)
		nzbpath = fmt.Sprintf("gps-%d_%s.nzb", time.Now().Unix(), altnzbpath)
		log.Infof("Using alternative filename: %s", nzbpath)
	}
	nzb := Nzb{}

	// Add some metadata
	if len(*nzbMetaPass) > 0 {
		nzb.Head = append(nzb.Head, Meta{Type: "password", Value: *nzbMetaPass})
	}

	for i, _ := range nzbinfo {
		n := nzbinfo[i]
		nzb.File = append(nzb.File, NzbFile{
			Poster:   n.Poster,
			Date:     n.Date,
			Subject:  n.Subject,
			Groups:   n.Groups,
			Segments: segs[i],
		})
	}

	err := CreateNzb(nzbpath, &nzb)
	if err != nil {
		log.Warningf("Error while creating Nzb: %s", err)
	}
	log.Infof("Generated Nzb file: %s", nzbpath)
	statusTicker.Stop()
}

// math.Min wants float64s, zzz
func min(a, b int64) int64 {
	if a < b {
		return a
	} else {
		return b
	}
}
