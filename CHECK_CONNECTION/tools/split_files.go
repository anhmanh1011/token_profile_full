package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: split_files <input_file> <num_parts> <output_prefix>")
		fmt.Println("Example: split_files emails.txt 4 emails_part")
		os.Exit(1)
	}

	inputFile := os.Args[1]
	numParts := 4
	fmt.Sscanf(os.Args[2], "%d", &numParts)
	outputPrefix := os.Args[3]

	// Count total lines
	totalLines, err := countLines(inputFile)
	if err != nil {
		log.Fatalf("Error counting lines: %v", err)
	}
	fmt.Printf("Total lines: %d\n", totalLines)

	linesPerPart := (totalLines + numParts - 1) / numParts
	fmt.Printf("Lines per part: ~%d\n", linesPerPart)

	// Open input file
	file, err := os.Open(inputFile)
	if err != nil {
		log.Fatalf("Error opening file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	currentPart := 1
	currentLine := 0
	var writer *bufio.Writer
	var outFile *os.File

	for scanner.Scan() {
		if currentLine%linesPerPart == 0 && currentPart <= numParts {
			// Close previous file
			if writer != nil {
				writer.Flush()
				outFile.Close()
			}

			// Open new file
			filename := fmt.Sprintf("%s%d.txt", outputPrefix, currentPart)
			outFile, err = os.Create(filename)
			if err != nil {
				log.Fatalf("Error creating file %s: %v", filename, err)
			}
			writer = bufio.NewWriter(outFile)
			fmt.Printf("Creating %s...\n", filename)
			currentPart++
		}

		writer.WriteString(scanner.Text() + "\n")
		currentLine++
	}

	if writer != nil {
		writer.Flush()
		outFile.Close()
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("Error reading file: %v", err)
	}

	fmt.Printf("Done! Created %d files\n", numParts)
}

func countLines(filename string) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	count := 0
	for scanner.Scan() {
		count++
	}
	return count, scanner.Err()
}
