package main

import (
	"encoding/binary"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
)

type block struct {
	filePath    string
	numBytes    uint16
	buffer      []byte
	startOfFile bool
	endOfFile   bool
}

var blockSize uint16
var verbose bool
var logger *log.Logger

const dataBlockFlag byte = 1 << 0
const startOfFileFlag byte = 1 << 1
const endOfFileFlag byte = 1 << 2

func main() {
	extract := flag.Bool("x", false, "extract archive")
	create := flag.Bool("c", false, "create archive")
	inputFileName := flag.String("i", "", "input file for extraction; defaults to stdin")
	outputFileName := flag.String("o", "", "output file for creation; defaults to stdout")
	requestedBlockSize := flag.Uint("block-size", 4096, "internal block-size, effective only during create archive")
	flag.BoolVar(&verbose, "v", false, "verbose output on stderr")
	flag.Parse()

	logger = log.New(os.Stderr, "", 0)

	if *requestedBlockSize > math.MaxUint16 {
		logger.Fatalln("block-size must be less than or equal to", math.MaxUint16)
	}
	blockSize = uint16(*requestedBlockSize)

	if *extract {
		var inputFile *os.File
		if *inputFileName != "" {
			file, err := os.Open(*inputFileName)
			if err != nil {
				logger.Fatalln("Error opening input file:", err.Error())
			}
			inputFile = file
		} else {
			inputFile = os.Stdin
		}

		archiveReader(inputFile)
		inputFile.Close()

	} else if *create {
		if flag.NArg() == 0 {
			logger.Fatalln("Directories to archive must be specified")
		}

		var directoryScanQueue = make(chan string, 128)
		var fileReadQueue = make(chan string, 128)
		var fileWriteQueue = make(chan block, 128)
		var workInProgress sync.WaitGroup

		var outputFile *os.File
		if *outputFileName != "" {
			file, err := os.Create(*outputFileName)
			if err != nil {
				logger.Fatalln("Error creating output file:", err.Error())
			}
			outputFile = file
		} else {
			outputFile = os.Stdout
		}

		go archiveWriter(outputFile, fileWriteQueue, &workInProgress)
		for i := 0; i < 16; i++ {
			go directoryScanner(directoryScanQueue, fileReadQueue, &workInProgress)
		}
		for i := 0; i < 16; i++ {
			go fileReader(fileReadQueue, fileWriteQueue, &workInProgress)
		}

		for i := 0; i < flag.NArg(); i++ {
			workInProgress.Add(1)
			directoryScanQueue <- flag.Arg(i)
		}

		workInProgress.Wait()
		close(directoryScanQueue)
		close(fileReadQueue)
		close(fileWriteQueue)
		outputFile.Close()
	} else {
		logger.Fatalln("extract (-x) or create (-c) flag must be provided")
	}
}

func directoryScanner(directoryScanQueue chan string, fileReadQueue chan string, workInProgress *sync.WaitGroup) {
	for directoryPath := range directoryScanQueue {
		if verbose {
			logger.Println(directoryPath)
		}

		files, err := ioutil.ReadDir(directoryPath)
		if err == nil {
			workInProgress.Add(len(files))
			for _, file := range files {
				filePath := filepath.Join(directoryPath, file.Name())
				if file.IsDir() {
					directoryScanQueue <- filePath
				} else {
					fileReadQueue <- filePath
				}
			}
		} else {
			logger.Println("directory read error:", err.Error())
		}

		workInProgress.Done()
	}
}

func fileReader(fileReadQueue <-chan string, fileWriterQueue chan block, workInProgress *sync.WaitGroup) {
	for filePath := range fileReadQueue {
		if verbose {
			logger.Println(filePath)
		}

		file, err := os.Open(filePath)
		if err == nil {
			workInProgress.Add(1)
			fileWriterQueue <- block{filePath, 0, nil, true, false}

			for {
				buffer := make([]byte, blockSize)
				bytesRead, err := file.Read(buffer)
				if err == io.EOF {
					break
				} else if err != nil {
					logger.Println("file read error; file contents will be incomplete:", err.Error())
					break
				}

				workInProgress.Add(1)
				fileWriterQueue <- block{filePath, uint16(bytesRead), buffer, false, false}
			}

			workInProgress.Add(1)
			fileWriterQueue <- block{filePath, 0, nil, false, true}

			file.Close()
		} else {
			logger.Println("file open error:", err.Error())
		}

		workInProgress.Done()
	}
}

func archiveWriter(output *os.File, fileWriterQueue <-chan block, workInProgress *sync.WaitGroup) {
	flags := make([]byte, 1)

	for block := range fileWriterQueue {
		filePath := []byte(block.filePath)
		err := binary.Write(output, binary.BigEndian, uint16(len(filePath)))
		if err != nil {
			logger.Panicln("Archive write error:", err.Error())
		}
		_, err = output.Write(filePath)
		if err != nil {
			logger.Panicln("Archive write error:", err.Error())
		}

		if block.startOfFile {
			flags[0] = startOfFileFlag
			_, err = output.Write(flags)
			if err != nil {
				logger.Panicln("Archive write error:", err.Error())
			}
		} else if block.endOfFile {
			flags[0] = endOfFileFlag
			_, err = output.Write(flags)
			if err != nil {
				logger.Panicln("Archive write error:", err.Error())
			}
		} else {
			flags[0] = dataBlockFlag
			_, err = output.Write(flags)
			if err != nil {
				logger.Panicln("Archive write error:", err.Error())
			}

			err = binary.Write(output, binary.BigEndian, uint16(block.numBytes))
			if err != nil {
				logger.Panicln("Archive write error:", err.Error())
			}

			_, err = output.Write(block.buffer[:block.numBytes])
			if err != nil {
				logger.Panicln("Archive write error:", err.Error())
			}
		}

		workInProgress.Done()
	}
}

func archiveReader(file *os.File) {
	var workInProgress sync.WaitGroup
	fileOutputChan := make(map[string]chan block)

	for {
		var pathSize uint16
		err := binary.Read(file, binary.BigEndian, &pathSize)
		if err == io.EOF {
			break
		} else if err != nil {
			logger.Panicln("Archive read error:", err.Error())
		}

		buf := make([]byte, pathSize)
		_, err = io.ReadFull(file, buf)
		if err != nil {
			logger.Panicln("Archive read error:", err.Error())
		}
		filePath := string(buf)

		flag := make([]byte, 1)
		_, err = io.ReadFull(file, flag)
		if err != nil {
			logger.Panicln("Archive read error:", err.Error())
		}

		if flag[0] == startOfFileFlag {
			c := make(chan block, 1)
			fileOutputChan[filePath] = c
			workInProgress.Add(1)
			go writeFile(c, &workInProgress)
			c <- block{filePath, 0, nil, true, false}
		} else if flag[0] == endOfFileFlag {
			c := fileOutputChan[filePath]
			c <- block{filePath, 0, nil, false, true}
			close(c)
			delete(fileOutputChan, filePath)
		} else if flag[0] == dataBlockFlag {
			var blockSize uint16
			err = binary.Read(file, binary.BigEndian, &blockSize)
			if err != nil {
				logger.Panicln("Archive read error:", err.Error())
			}

			blockData := make([]byte, blockSize)
			_, err = io.ReadFull(file, blockData)
			if err != nil {
				logger.Panicln("Archive read error:", err.Error())
			}

			c := fileOutputChan[filePath]
			c <- block{filePath, blockSize, blockData, false, false}
		} else {
			logger.Panicln("Archive error: unrecognized block flag", flag[0])
		}
	}

	file.Close()
	workInProgress.Wait()
}

func writeFile(blockSource chan block, workInProgress *sync.WaitGroup) {
	var file *os.File = nil
	for block := range blockSource {
		if block.startOfFile {
			if verbose {
				logger.Println(block.filePath)
			}

			dir, _ := filepath.Split(block.filePath)
			err := os.MkdirAll(dir, os.ModeDir|0755)
			if err != nil {
				logger.Panicln("Directory create error:", err.Error())
			}

			tmp, err := os.Create(block.filePath)
			if err != nil {
				logger.Panicln("File create error:", err.Error())
			}
			file = tmp
		} else if block.endOfFile {
			file.Close()
			file = nil
		} else {
			_, err := file.Write(block.buffer[:block.numBytes])
			if err != nil {
				logger.Panicln("File write error:", err.Error())
			}
		}
	}
	workInProgress.Done()
}
