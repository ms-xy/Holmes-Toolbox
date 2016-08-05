package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"time"
	"path/filepath"

	"gopkg.in/mgo.v2/bson"
	"sync"

	"github.com/rakyll/magicmime"
	"strings"
)

type critsSample struct {
	Id  bson.ObjectId `json:"_id" bson:"_id,omitempty"`
	MD5 string        `json:"md5"`
}

var (
	client          *http.Client
	critsFileServer string
	holmesStorage   string
	directory       string
	comment         string
	userid          string
	source          string
	mimetypePattern string
	topLevel        bool
	recursive       bool
	insecure        bool
	numWorkers      int
	wg              sync.WaitGroup
	c               chan string
)

func init() {
	// http client
	tr := &http.Transport{}
	/*
		// Disable SSL verification
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	*/

	client = &http.Client{Transport: tr}
}

func worker() {
	for true{
		sample :=<- c
		//log.Printf("Working on %s\n", sample)
		copySample(sample)
		wg.Done()
	}
}

func main() {
	log.Println("Running...")

	// cmd line flags
	var fPath string
	flag.StringVar(&fPath, "file", "", "List of samples (MD5, SHAX, CRITs ID) to upload. Files are first searched locally. If they are not found and a CRITs file server is specified, they are taken from there. (optional)")
	flag.StringVar(&critsFileServer, "cfs", "", "Full URL to your CRITs file server, as a fallback (optional)")
	flag.StringVar(&holmesStorage, "storage", "", "Full URL to your Holmes-Storage server, i.e. 'http://storage:8080'")
	flag.StringVar(&mimetypePattern, "mime", "", "Only upload files with the specified mime-type (as substring)")
	flag.StringVar(&directory, "dir", "", "Directory of samples to upload")
	flag.StringVar(&comment, "comment", "", "Comment of submitter")
	flag.StringVar(&source, "src", "", "Source information for the files")
	flag.StringVar(&userid, "uid", "-1", "User ID of submitter")
	flag.IntVar(&numWorkers, "workers", 1, "Number of parallel workers")
	flag.BoolVar(&recursive, "rec", false, "If set, the directory specified with \"-dir\" will be iterated recursively")
	flag.BoolVar(&insecure, "insecure", false, "If set, disables certificate checking")
	flag.Parse()

	//log.Sprintf("Copying samples from %s", fPath)

	c = make(chan string)
	for i := 0; i < numWorkers; i++ {
		go worker()
	}

	if fPath != "" {
		file, err := os.Open(fPath)
		if err != nil {
			log.Println("Couln't open file containing sample list!", err.Error())
			return
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		scanner.Split(bufio.ScanLines)
		// line by line
		for scanner.Scan() {
			wg.Add(1)
			c <- scanner.Text()
			//go copySample(scanner.Text())
		}
	}
	if directory != "" {
		magicmime.Open(magicmime.MAGIC_MIME_TYPE | magicmime.MAGIC_SYMLINK | magicmime.MAGIC_ERROR)
		defer magicmime.Close()

		fullPath, err := filepath.Abs(directory)

		if err != nil {
			log.Println("path error:", err)
			return
		}
		topLevel = true
		err = filepath.Walk(fullPath, walkFn)
		if err != nil {
			log.Println("walk error:", err)
			return
		}
	}
	wg.Wait()
}

func walkFn(path string, fi os.FileInfo, err error) (error) {
	if fi.IsDir(){
		if recursive {
			return nil
		} else {
			if topLevel {
				topLevel = false
				return nil
			} else {
				return filepath.SkipDir
			}
		}
	}
	mimetype, err := magicmime.TypeByFile(path)
	if err != nil {
		log.Println("mimetype error (skipping " + path + "):", err)
		return nil
	}
	if strings.Contains(mimetype, mimetypePattern) {
		log.Println("Adding " + path + " (" + mimetype + ")")
		wg.Add(1)
		c <- path
		return nil
	} else {
		log.Println("Skipping " + path + " (" + mimetype + ")")
		return nil
	}
}

func copySample(name string) {
	// set all necessary parameters
	parameters := map[string]string{
		"user_id": userid, // user id of uploader (command line argument)
		"source":  source, // (TODO) Gateway should match existing sources (command line argument)
		"name":    filepath.Base(name), // filename
		"date":    time.Now().Format(time.RFC3339),
		"comment": comment, // comment from submitter (command line argument)
		//"tags"
	}

	request, err := buildRequest(holmesStorage+"/samples/", parameters, name)

	if err != nil {
		log.Fatal("ERROR: " + err.Error())
	}

	tr := &http.Transport{}
	if insecure{
		// Disable SSL verification
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	client = &http.Client{Transport: tr}
	resp, err := client.Do(request)
	if err != nil {
		log.Fatal("ERROR: " + err.Error())
		return
	}

	body := &bytes.Buffer{}
	_, err = body.ReadFrom(resp.Body)
	if err != nil {
		log.Fatal("ERROR: " + err.Error())
	}
	resp.Body.Close()

	log.Println("Uploaded", name)
	log.Println(resp.StatusCode)
	log.Println(body)
	log.Println("-------------------------------------------")
}

func buildRequest(uri string, params map[string]string, hash string) (*http.Request, error) {
	var r io.Reader

	// check if local file
	r, err := os.Open(hash)
	if err != nil {
		// not a local file
		// try to get file from crits file server

		cId := &critsSample{}
		if err := bson.Unmarshal([]byte(hash), cId); err != nil {
			return nil, err
		}
		rawId := cId.Id.Hex()

		resp, err := client.Get(critsFileServer + "/" + rawId)
		if err != nil {
			return nil, err
		}
		defer SafeResponseClose(resp)

		// return if file does not exist
		if resp.StatusCode != 200 {
			return nil, errors.New("Couldn't download file")
		}

		r = resp.Body
		// For files coming from CRITs: TODO: find real name somehow
	}

	// build Holmes-Storage PUT request
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("sample", hash)
	if err != nil {
		return nil, err
	}

	_, err = io.Copy(part, r)
	if err != nil {
		return nil, err
	}

	for key, val := range params {
		err = writer.WriteField(key, val)
		if err != nil {
			return nil, err
		}
	}

	err = writer.Close()
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequest("PUT", uri, body)
	//request, err := http.NewRequest("POST", uri, body)
	if err != nil {
		return nil, err
	}
	request.Header.Add("Content-Type", writer.FormDataContentType())

	return request, nil
}

func SafeResponseClose(r *http.Response) {
	if r == nil {
		return
	}

	io.Copy(ioutil.Discard, r.Body)
	r.Body.Close()
}
