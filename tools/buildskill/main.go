package main

import (
	"archive/zip"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// buildskill bundles skill/status-orchestrator/ into static/status-orchestrator.skill (a zip).
// Run from repo root: go run ./tools/buildskill
func main() {
	srcDir := filepath.Join("skill", "status-orchestrator")
	outPath := filepath.Join("static", "status-orchestrator.skill")

	out, err := os.Create(outPath)
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	zw := zip.NewWriter(out)
	defer zw.Close()

	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Dir(srcDir), path)
		if err != nil {
			return err
		}
		rel = strings.ReplaceAll(rel, "\\", "/")

		fh, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		fh.Name = rel
		fh.Method = zip.Deflate
		w, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", outPath)
}
