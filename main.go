// Download and unpack a store path from binary cache
// Go is used to parse the NAR archive format

// Hacked together based on code from https://github.com/numtide/nar-serve/blob/master/api/unpack/index.go
// As such this program shall be covered under the same license

package main

import (
	"compress/bzip2"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/nix-community/go-nix/pkg/nar"
	"github.com/nix-community/go-nix/pkg/nar/narinfo"
	"github.com/numtide/nar-serve/libstore"
	"github.com/ulikunitz/xz"
)

var nixCache = mustBinaryCacheReader()

func mustBinaryCacheReader() libstore.BinaryCacheReader {
	r, err := libstore.NewBinaryCacheReader(context.Background(), "https://cache.nixos.org")
	if err != nil {
		panic(err)
	}
	return r
}

func getNarInfo(ctx context.Context, key string) (*narinfo.NarInfo, error) {
	path := fmt.Sprintf("%s.narinfo", key)
	fmt.Println("Fetching the narinfo:", path, "from:", nixCache.URL())
	r, err := nixCache.GetFile(ctx, path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	ni, err := narinfo.Parse(r)
	if err != nil {
		return nil, err
	}
	return ni, err
}

type HydraOutput struct {
	Path string `json:"path"`
}

type HydraBuildOutputs struct {
	Out HydraOutput `json:"out"`
}

type HydraJob struct {
	BuildOutputs HydraBuildOutputs `json:"buildoutputs"`
}

func getLatestOutputPath(hydraJob string) (string, error) {
	url := fmt.Sprintf("https://hydra.nixos.org/job/%s/latest", hydraJob)
	hydraClient := http.Client{}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	res, err := hydraClient.Do(req)
	if err != nil {
		return "", err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	body, readErr := ioutil.ReadAll(res.Body)
	if readErr != nil {
		return "", err
	}

	job := HydraJob{}
	err = json.Unmarshal(body, &job)
	if err != nil {
		return "", err
	}

	if job.BuildOutputs.Out.Path == "" {
		return "", fmt.Errorf("Couldn't find a valid store path for job: %s", hydraJob)
	}

	return job.BuildOutputs.Out.Path, nil
}

func main() {
	ctx := context.Background()

	outputFile := os.Args[2]

	// Execution of CLI tool behavior goes here
	hydraJob := os.Args[1]
	fmt.Println("Getting latest artifact for:", hydraJob)

	storePath, err := getLatestOutputPath(hydraJob)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Store path:", storePath)

	// remove the mount path from the path
	path := strings.TrimPrefix(storePath, "/nix/store/")
	// ignore trailing slashes
	path = strings.TrimRight(path, "/")
	components := strings.Split(path, "/")
	storeHash := strings.Split(components[0], "-")[0]

	// Get the NAR info to find the NAR
	narinfo, err := getNarInfo(ctx, storeHash)
	if err != nil {
		log.Fatal(err)
	}
	// fmt.Println("narinfo", narinfo)

	// TODO: consider keeping a LRU cache
	narPATH := narinfo.URL
	fmt.Println("fetching the NAR:", narPATH)

	file, err := nixCache.GetFile(ctx, narPATH)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	var r io.Reader
	r = file

	// decompress on the fly
	switch narinfo.Compression {
	case "xz":
		r, err = xz.NewReader(r)
		if err != nil {
			log.Fatal(err)
		}
	case "bzip2":
		r = bzip2.NewReader(r)
	default:
		log.Fatal(fmt.Sprintf("compression %s not handled", narinfo.Compression))
		return
	}

	// TODO: try to load .ls files to speed-up the file lookups

	narReader, err := nar.NewReader(r)
	if err != nil {
		log.Fatal(err)
	}

	newPath := strings.Join(strings.Split(components[0], "-")[1:], "-")

	fmt.Println("newPath", newPath)

	for {
		hdr, err := narReader.Next()
		fmt.Println("Iterating on ", hdr.Path)
		if err != nil {
			if err == io.EOF {
				log.Fatal("file not found")
			} else {
				log.Fatal(err)
			}
			return
		}

		// we've got a match!
		path := strings.Split(hdr.Path, "/")
		if path[len(path)-1] == newPath {
			switch hdr.Type {
			case nar.TypeRegular:
				f, err := os.Create(outputFile)
				if err != nil {
					log.Fatal(err)
				}
				defer f.Close()
				io.CopyN(f, narReader, hdr.Size)
				f.Sync()
				fmt.Println("Wrote", hdr.Path, "to", outputFile)
				return
			default:
				// Support for more complex file structures has been removed for simplicity
				log.Fatal(fmt.Sprintf("BUG: unknown NAR header type: %s", hdr.Type))
			}
			return
		}
	}
}
