package main

import (
    "archive/tar"
    "archive/zip"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "strings"
    "sync"
    "time"

    "github.com/klauspost/compress/zstd"
    "github.com/ulikunitz/xz"
)

type NodeVersion struct {
    Version string      `json:"version"`
    LTS     interface{} `json:"lts"`
}

var targets = map[string]string{
    "node_darwin_amd64.zst":  "darwin-x64",
    "node_darwin_arm64.zst":  "darwin-arm64",
    "node_linux_amd64.zst":   "linux-x64",
    "node_linux_arm64.zst":   "linux-arm64",
    "node_linux_armv7.zst":   "linux-armv7l",
    "node_windows_amd64.zst": "win-x64",
    "node_windows_arm64.zst": "win-arm64",
    "node_windows_i386.zst":  "win-x86",
}

// 进度条 Writer
type ProgressWriter struct {
    Total      int64
    Written    int64
    LastUpdate time.Time
    Prefix     string
}

func (pw *ProgressWriter) Write(p []byte) (int, error) {
    n := len(p)
    pw.Written += int64(n)
    now := time.Now()
    if now.Sub(pw.LastUpdate) > 300*time.Millisecond {
        pw.LastUpdate = now
        percent := float64(pw.Written) / float64(pw.Total) * 100
        fmt.Printf("\r%s %.1f%%", pw.Prefix, percent)
    }
    return n, nil
}

func main() {
    version, err := fetchLatestLTS()
    if err != nil {
        panic(err)
    }
    fmt.Println("最新 LTS 版本:", version)

    var wg sync.WaitGroup
    wg.Add(len(targets))

    sem := make(chan struct{}, 3) // 限制最大并发数为3

    for outFile, platform := range targets {
        go func(outFile, platform string) {
            defer wg.Done()
            sem <- struct{}{}
            defer func() { <-sem }()

            if err := processTarget(version, outFile, platform); err != nil {
                fmt.Printf("\n❌ %s 失败: %v\n", outFile, err)
            } else {
                fmt.Printf("\n✅ 完成: %s\n", outFile)
            }
        }(outFile, platform)
    }

    wg.Wait()
    fmt.Println("\n🎉 全部完成")
}

func fetchLatestLTS() (string, error) {
    resp, err := http.Get("https://nodejs.org/dist/index.json")
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    var versions []NodeVersion
    if err := json.NewDecoder(resp.Body).Decode(&versions); err != nil {
        return "", err
    }
    for _, v := range versions {
        if v.LTS != false && v.LTS != nil {
            return v.Version, nil
        }
    }
    return "", fmt.Errorf("未找到 LTS 版本")
}

func processTarget(version, outFile, platform string) error {
    url := buildURL(version, platform)
    fmt.Printf("\n⬇️  下载 %s -> %s\n", url, outFile)

    tmpFile := outFile + ".tmp"
    if err := downloadFile(tmpFile, url, platform); err != nil {
        return err
    }
    defer os.Remove(tmpFile)

    exeFile := outFile + ".nodebin"
    if strings.HasPrefix(platform, "win") {
        if err := extractNodeFromZip(tmpFile, exeFile, platform); err != nil {
            return err
        }
    } else {
        if err := extractNodeFromTarXZ(tmpFile, exeFile, platform); err != nil {
            return err
        }
    }

    if err := compressZstd(exeFile, outFile, platform); err != nil {
        return err
    }
    os.Remove(exeFile)
    return nil
}

func buildURL(version, platform string) string {
    ext := ".tar.xz"
    if strings.HasPrefix(platform, "win") {
        ext = ".zip"
    }
    return fmt.Sprintf("https://nodejs.org/dist/%s/node-%s-%s%s",
        version, version, platform, ext)
}

func downloadFile(filename, url, platform string) error {
    resp, err := http.Get(url)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    out, err := os.Create(filename)
    if err != nil {
        return err
    }
    defer out.Close()

    pw := &ProgressWriter{Total: resp.ContentLength, Prefix: "下载[" + platform + "]"}
    _, err = io.Copy(out, io.TeeReader(resp.Body, pw))
    fmt.Printf("\r下载[%s] 100%%\n", platform)
    return err
}

func extractNodeFromZip(zipPath, outFile, platform string) error {
    r, err := zip.OpenReader(zipPath)
    if err != nil {
        return err
    }
    defer r.Close()

    for _, f := range r.File {
        if strings.HasSuffix(f.Name, "node.exe") {
            rc, err := f.Open()
            if err != nil {
                return err
            }
            defer rc.Close()

            out, err := os.Create(outFile)
            if err != nil {
                return err
            }
            defer out.Close()

            _, err = io.Copy(out, rc)
            fmt.Printf("解压[%s] node.exe 完成\n", platform)
            return err
        }
    }
    return fmt.Errorf("未找到 node.exe")
}

func extractNodeFromTarXZ(tarxzPath, outFile, platform string) error {
    f, err := os.Open(tarxzPath)
    if err != nil {
        return err
    }
    defer f.Close()

    xzr, err := xz.NewReader(f)
    if err != nil {
        return err
    }
    tr := tar.NewReader(xzr)

    for {
        h, err := tr.Next()
        if err == io.EOF {
            break
        }
        if err != nil {
            return err
        }
        if strings.HasSuffix(h.Name, "/bin/node") {
            out, err := os.Create(outFile)
            if err != nil {
                return err
            }
            defer out.Close()

            _, err = io.Copy(out, tr)
            fmt.Printf("解压[%s] bin/node 完成\n", platform)
            return err
        }
    }
    return fmt.Errorf("未找到 bin/node")
}

func compressZstd(input, output, platform string) error {
    in, err := os.Open(input)
    if err != nil {
        return err
    }
    defer in.Close()

    info, _ := in.Stat()
    out, err := os.Create(output)
    if err != nil {
        return err
    }
    defer out.Close()

    enc, err := zstd.NewWriter(out)
    if err != nil {
        return err
    }
    defer enc.Close()

    pw := &ProgressWriter{Total: info.Size(), Prefix: "压缩[" + platform + "]"}
    _, err = io.Copy(enc, io.TeeReader(in, pw))
    fmt.Printf("\r压缩[%s] 100%%\n", platform)
    return err
}
