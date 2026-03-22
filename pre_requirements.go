package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

type dependency struct {
	Name    string
	Command string
}

// CheckPrerequisites verifies that all system dependencies are installed.
func CheckPrerequisites() {
	deps := []dependency{
		{Name: "ImageMagick", Command: "convert -version"},
	}

	var missing []string

	fmt.Println("🔍 Checking system prerequisites...")

	for _, dep := range deps {
		// Split command to pass to exec.Command
		parts := strings.Fields(dep.Command)
		var cmd *exec.Cmd
		if len(parts) > 1 {
			cmd = exec.Command(parts[0], parts[1:]...)
		} else {
			cmd = exec.Command(parts[0])
		}

		if err := cmd.Run(); err != nil {
			missing = append(missing, dep.Name)
			fmt.Printf("❌ %s is NOT found.\n", dep.Name)
		} else {
			fmt.Printf("✅ %s is installed.\n", dep.Name)
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
		fmt.Printf("  sudo apt-get install -y imagemagick\n")
	}
	fmt.Printf("%s\n\n", sep)
}
