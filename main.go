package main

import (
	"flag"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/adrium/goheif"
	"github.com/danbrakeley/frog"
)

// (go tool nm) -ldflags '-X "main.Version=${{version}}"'
var Version string = "unknown"

// (go tool nm) -ldflags '-X "main.BuildTimestamp=${{date -u +"%Y-%m-%dT%H:%M:%SZ"}}"'
var BuildTimestamp string = "unknown"

func main() {
	// this one line main() ensures os.Exit is only called after all defers have run
	os.Exit(main_())
}

func main_() int {
	var source string
	var recursive bool
	var targetDir string
	var numWorkers int
	var deleteOnSuccess bool
	var forceOverwrite bool
	var version bool
	flag.StringVar(&source, "source", "", "")
	flag.StringVar(&source, "s", "", "")
	flag.BoolVar(&recursive, "recursive", false, "")
	flag.StringVar(&targetDir, "target-dir", ".", "")
	flag.StringVar(&targetDir, "t", ".", "")
	flag.BoolVar(&deleteOnSuccess, "delete", false, "")
	flag.BoolVar(&forceOverwrite, "overwrite", false, "")
	flag.IntVar(&numWorkers, "procs", runtime.NumCPU(), "")
	flag.IntVar(&numWorkers, "p", runtime.NumCPU(), "")
	flag.BoolVar(&version, "version", false, "")
	flag.BoolVar(&version, "v", false, "")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage of heic2png:
  -s, --source <path>       specify a source file or directory.
  -r  --recursive           recurse into subdirectories, if source is a directory
  -t, --target-dir <path>   specify a target directory. If it does not exist, it gets created
      --delete  · · · · ·   delete original heic file after successfull conversion
      --overwrite · · · ·   if target png file already exists, overwrite it
  -p, --procs   <num_procs> max number of files to process in parallel (default %d)
  -v, --version · · · · ·   print version information and exit
  -h, --help    · · · · ·   print this message and exit
`, runtime.NumCPU())
	}
	flag.Parse()

	// print version info and exit
	if version {
		fmt.Printf("Version: %s\nBuilt On: %s\n", Version, BuildTimestamp)
		return 0
	}

	if len(source) == 0 {
		fmt.Fprintf(os.Stderr, "no file(s) specified\n")
		flag.Usage()
		return -1
	}

	log := frog.New(frog.Auto, frog.POFieldIndent(30))
	defer log.Close()

	// build list of files
	var filelist []string
	var directoryQueue []string
	
	// check if source path exists and determine if it's a file or directory
	sourceInfo, err := os.Stat(source)
	if err != nil {
		log.Error("source path does not exist or is not accessible", frog.String("source", source), frog.Err(err))
		return -1
	}
	
	if sourceInfo.IsDir() {
		directoryQueue = append(directoryQueue, source)
	} else {
		filelist = append(filelist, source)
	}

	for len(directoryQueue) > 0 {
		dir := directoryQueue[0]
		directoryQueue = directoryQueue[1:]

		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Error("unable to read directory", frog.String("directory", dir), frog.Err(err))
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				if recursive {
					directoryQueue = append(directoryQueue, filepath.Join(dir, entry.Name()))
					continue
				}
			} else {
				if strings.HasSuffix(strings.ToLower(entry.Name()), ".heic") {
					filelist = append(filelist, filepath.Join(dir, entry.Name()))
				}
			}
		}
	}

	if len(filelist) == 0 {
		log.Warning("no files found, nothing to do")
		return 0
	} else if len(filelist) > 1 {
		log.Info("found files", frog.Int("num_files", len(filelist)))
	}

	// only spin up as many workers as we'll actually need
	if numWorkers > len(filelist) {
		numWorkers = len(filelist)
	}

	var wg sync.WaitGroup
	chTasks := make(chan Task)
	var numErrs int32

	// spin up workers
	log.Info("starting", frog.Int("num_workers", numWorkers), frog.String("version", Version), frog.String("built_on", BuildTimestamp))
	wg.Add(numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func(thread int) {
			defer wg.Done()
			l := frog.AddAnchor(log)
			defer frog.RemoveAnchor(l)

			// loop until channel is closed
			for t := range chTasks {
				fid := frog.Int("worker_id", thread)
				fheic := frog.String("heic_path", t.HeicPath)
				fpng := frog.String("png_path", t.PngPath)

				err := convertHeicToPng(t.HeicPath, t.PngPath, t.TargetDir, forceOverwrite, func(step, max int) {
					l.Transient("converting: "+progressBar(step, max, '·', '☆'), fheic, fpng, fid)
				})
				if err != nil {
					l.Error("conversion failed", fheic, fpng, frog.Err(err), fid)
					atomic.AddInt32(&numErrs, 1)
					continue
				}

				l.Info("image converted", fheic, fpng, fid)

				if deleteOnSuccess {
					l.Transient("deleting", fheic, fid)

					if err = os.Remove(t.HeicPath); err != nil {
						l.Error("delete failed", fheic, frog.Err(err), fid)
						atomic.AddInt32(&numErrs, 1)
						continue
					}
					l.Info("original image deleted", fheic, fid)
				}
			}
		}(i)
	}

	// send work to workers
	for _, v := range filelist {
		chTasks <- Task{
			HeicPath:  v,
			PngPath:   removeExt(v) + ".png",
			TargetDir: targetDir,
		}
	}

	close(chTasks)
	wg.Wait()
	log.Info("done", frog.Int32("num_errors", numErrs))

	// use the worker error count as the exit status
	return int(numErrs)
}

type Task struct {
	HeicPath  string
	PngPath   string
	TargetDir string
}

func convertHeicToPng(filenameIn, filenameOut string, targetDir string, forceOverwrite bool, fnProgress func(step, max int)) error {
	if fnProgress == nil {
		fnProgress = func(_, _ int) {}
	}

	fnProgress(0, 4)
	fIn, err := os.Open(filenameIn)
	if err != nil {
		return fmt.Errorf("unable to open %s: %w", filenameIn, err)
	}
	defer fIn.Close()

	fnProgress(1, 4)
	img, err := goheif.Decode(fIn)
	if err != nil {
		return fmt.Errorf("unable to decode %s: %w", filenameIn, err)
	}

	fnProgress(2, 4)
	var fOut *os.File
	var outputPath string
	
	// If filenameOut is absolute, use it directly; otherwise join with targetDir
	if filepath.IsAbs(filenameOut) {
		outputPath = filenameOut
	} else {
		outputPath = filepath.Join(targetDir, filenameOut)
	}

	// extract directory from output path and create all directories
	outputDir := filepath.Dir(outputPath)
	err = os.MkdirAll(outputDir, 0o755)
	if err != nil {
		return fmt.Errorf("unable to create directory %s: %w", outputDir, err)
	}

	if forceOverwrite {
		fOut, err = os.Create(outputPath)
	} else {
		fOut, err = os.OpenFile(outputPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
	}
	if err != nil {
		return fmt.Errorf("unable to create %s: %v", outputPath, err)
	}
	defer fOut.Close()

	fnProgress(3, 4)
	pngenc := png.Encoder{CompressionLevel: png.BestSpeed}
	err = pngenc.Encode(fOut, img)
	if err != nil {
		return fmt.Errorf("unable to encode %s: %w", outputPath, err)
	}

	fnProgress(4, 4)
	return nil
}

func progressBar(cur, max int, empty, full rune) string {
	var sb strings.Builder
	sb.Grow(max * 4)
	for i := 0; i < max; i++ {
		if i < cur {
			sb.WriteRune(full)
		} else {
			sb.WriteRune(empty)
		}
	}
	return sb.String()
}

func removeExt(p string) string {
	return strings.TrimSuffix(p, filepath.Ext(p))
}
