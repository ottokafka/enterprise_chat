package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

var (
	convertBinary   string
	convertBinaryMu sync.Mutex
)

// resolveConvertBinary finds the ImageMagick convert binary, checking PATH and
// common install locations. systemd services often run with a minimal PATH.
func resolveConvertBinary() (string, error) {
	convertBinaryMu.Lock()
	defer convertBinaryMu.Unlock()

	if convertBinary != "" {
		if _, err := os.Stat(convertBinary); err == nil {
			return convertBinary, nil
		}
		convertBinary = ""
	}

	for _, name := range []string{"convert", "magick"} {
		if path, err := exec.LookPath(name); err == nil {
			convertBinary = path
			return convertBinary, nil
		}
	}

	for _, path := range []string{
		"/usr/bin/convert",
		"/usr/bin/convert-im6.q16",
		"/usr/local/bin/convert",
	} {
		if _, err := os.Stat(path); err == nil {
			convertBinary = path
			return convertBinary, nil
		}
	}

	return "", fmt.Errorf("ImageMagick convert not found in PATH")
}

// CheckPrerequisites verifies that all system dependencies are installed.
func CheckPrerequisites() {
	var missing []string

	fmt.Println("🔍 Checking system prerequisites...")

	if convertPath, err := resolveConvertBinary(); err != nil {
		missing = append(missing, "ImageMagick")
		fmt.Printf("❌ ImageMagick is NOT found.\n")
	} else {
		cmd := exec.Command(convertPath, "-version")
		if err := cmd.Run(); err != nil {
			missing = append(missing, "ImageMagick")
			fmt.Printf("❌ ImageMagick is NOT found.\n")
		} else {
			fmt.Printf("✅ ImageMagick is installed (%s).\n", convertPath)
		}
	}

	if len(missing) > 0 {
		printInstallInstructions(missing)
	} else {
		fmt.Printf("🚀 All systems go! Proceeding with file processing.\n\n")
	}
}

func printInstallInstructions(missing []string) {
	isMac := runtime.GOOS == "darwin"
	isLinux := runtime.GOOS == "linux"

	sep := strings.Repeat("=", 50)
	fmt.Printf("\n%s\n", sep)
	fmt.Printf("🚨 MISSING DEPENDENCIES: %s\n", strings.Join(missing, ", "))
	fmt.Printf("Please install the requirements to handle PDF-to-Image conversion.\n")
	fmt.Printf("%s\n", sep)

	if isMac {
		fmt.Printf("\n🍎 FOR MAC OS (The 'Rich' way):\n")
		fmt.Printf("  brew update\n")
		fmt.Printf("  brew install imagemagick\n")
	} else if isLinux {
		fmt.Printf("\n🐧 FOR LINUX (Debian/Ubuntu):\n")
		fmt.Printf("  sudo apt-get update\n")
		fmt.Printf("  sudo apt-get install -y imagemagick ghostscript\n")
	}
	fmt.Printf("%s\n\n", sep)
}
