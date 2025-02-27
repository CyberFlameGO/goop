package goop

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/deletescape/goop/internal/jobtracker"
	"github.com/deletescape/goop/internal/utils"
	"github.com/deletescape/goop/internal/workers"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
	"github.com/phuslu/log"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"
)

func proxyFromEnv() fasthttp.DialFunc {
	allProxy, okAll := os.LookupEnv("all_proxy")
	httpProxy, okHttp := os.LookupEnv("http_proxy")
	httpsProxy, okHttps := os.LookupEnv("https_proxy")

	uriToDial := func(u string) fasthttp.DialFunc {
		pUri, err := url.Parse(u)
		if err != nil {
			panic(err) // TODO: uh, handle better
		}
		if pUri.Scheme == "socks5" {
			return fasthttpproxy.FasthttpSocksDialer(pUri.Host)
		}
		return fasthttpproxy.FasthttpHTTPDialer(pUri.Host) // this probly doesnt work for proxys with auth rn
	}

	if okAll {
		return uriToDial(allProxy)
	}
	if okHttp {
		return uriToDial(httpProxy)
	}
	if okHttps {
		return uriToDial(httpsProxy)
	}

	return nil
}

var c = &fasthttp.Client{
	Name:            "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/85.0.4183.102 Safari/537.36",
	MaxConnsPerHost: utils.MaxInt(maxConcurrency+250, fasthttp.DefaultMaxConnsPerHost),
	TLSConfig: &tls.Config{
		InsecureSkipVerify: true,
	},
	NoDefaultUserAgentHeader: true,
	MaxConnWaitTimeout:       10 * time.Second,
	Dial:                     proxyFromEnv(),
}

func CloneList(listFile, baseDir string, force, keep bool) error {
	lf, err := os.Open(listFile)
	if err != nil {
		return err
	}
	defer lf.Close()

	listScan := bufio.NewScanner(lf)
	for listScan.Scan() {
		u := listScan.Text()
		if u == "" {
			continue
		}
		dir := baseDir
		if dir != "" {
			parsed, err := url.Parse(u)
			if err != nil {
				log.Error().Str("uri", u).Err(err).Msg("couldn't parse uri")
				continue
			}
			dir = utils.Url(dir, parsed.Host)
		}
		log.Info().Str("target", u).Str("dir", dir).Bool("force", force).Bool("keep", keep).Msg("starting download")
		if err := Clone(u, dir, force, keep); err != nil {
			log.Error().Str("target", u).Str("dir", dir).Bool("force", force).Bool("keep", keep).Msg("download failed")
		}
	}
	return nil
}

func Clone(u, dir string, force, keep bool) error {
	baseUrl := strings.TrimSuffix(u, "/")
	baseUrl = strings.TrimSuffix(baseUrl, "/HEAD")
	baseUrl = strings.TrimSuffix(baseUrl, "/.git")
	parsed, err := url.Parse(baseUrl)
	if err != nil {
		return err
	}
	if parsed.Scheme == "" {
		parsed.Scheme = "http"
	}
	baseUrl = parsed.String()
	parsed, err = url.Parse(baseUrl)
	if err != nil {
		return err
	}
	baseDir := dir
	if baseDir == "" {
		baseDir = parsed.Host
	}

	if utils.Exists(baseDir) {
		if !utils.IsFolder(baseDir) {
			return fmt.Errorf("%s is not a directory", baseDir)
		}
		isEmpty, err := utils.IsEmpty(baseDir)
		if err != nil {
			return err
		}
		if !isEmpty {
			if force {
				if err := os.RemoveAll(baseDir); err != nil {
					return err
				}
			} else if !keep {
				return fmt.Errorf("%s is not empty", baseDir)
			}
		}
	}

	return FetchGit(baseUrl, baseDir)
}

func FetchGit(baseUrl, baseDir string) error {
	log.Info().Str("base", baseUrl).Msg("testing for .git/HEAD")
	code, body, err := c.Get(nil, utils.Url(baseUrl, ".git/HEAD"))
	if err != nil {
		return err
	}

	if code != 200 {
		log.Warn().Str("base", baseUrl).Int("code", code).Msg(".git/HEAD doesn't appear to exist, clone will most likely fail")
	} else if !bytes.HasPrefix(body, refPrefix) {
		log.Warn().Str("base", baseUrl).Int("code", code).Msg(".git/HEAD doesn't appear to be a git HEAD file, clone will most likely fail")
	}

	log.Info().Str("base", baseUrl).Msg("testing if recursive download is possible")
	code, body, err = c.Get(body, utils.Url(baseUrl, ".git/"))
	if err != nil {
		if utils.IgnoreError(err) {
			log.Error().Str("base", baseUrl).Int("code", code).Err(err)
		} else {
			return err
		}
	}

	if code == 200 && utils.IsHtml(body) {
		lnk, _ := url.Parse(utils.Url(baseUrl, ".git/"))
		indexedFiles, err := utils.GetIndexedFiles(body, lnk.Path)
		if err != nil {
			return err
		}
		if utils.StringsContain(indexedFiles, "HEAD") {
			log.Info().Str("base", baseUrl).Msg("fetching .git/ recursively")
			jt := jobtracker.NewJobTracker()
			for _, f := range indexedFiles {
				// TODO: add support for non top level git repos
				jt.AddJob(utils.Url(".git", f))
			}
			for w := 1; w <= maxConcurrency; w++ {
				go workers.RecursiveDownloadWorker(c, baseUrl, baseDir, jt)
			}
			jt.StartAndWait()

			log.Info().Str("dir", baseDir).Msg("running git checkout .")
			cmd := exec.Command("git", "checkout", ".")
			cmd.Dir = baseDir
			return cmd.Run()
		}
	}

	log.Info().Str("base", baseUrl).Msg("fetching common files")
	jt := jobtracker.NewJobTracker()
	for _, f := range commonFiles {
		jt.AddJob(f)
	}
	concurrency := utils.MinInt(maxConcurrency, len(commonFiles))
	for w := 1; w <= concurrency; w++ {
		go workers.DownloadWorker(c, baseUrl, baseDir, jt, false, false)
	}
	jt.StartAndWait()

	jt = jobtracker.NewJobTracker()
	log.Info().Str("base", baseUrl).Msg("finding refs")
	for _, ref := range commonRefs {
		jt.AddJob(ref)
	}
	for w := 1; w <= maxConcurrency; w++ {
		go workers.FindRefWorker(c, baseUrl, baseDir, jt)
	}
	jt.StartAndWait()

	log.Info().Str("base", baseUrl).Msg("finding packs")
	infoPacksPath := utils.Url(baseDir, ".git/objects/info/packs")
	if utils.Exists(infoPacksPath) {
		infoPacks, err := ioutil.ReadFile(infoPacksPath)
		if err != nil {
			return err
		}
		hashes := packRegex.FindAllSubmatch(infoPacks, -1)
		jt = jobtracker.NewJobTracker()
		for _, sha1 := range hashes {
			jt.AddJob(fmt.Sprintf(".git/objects/pack/pack-%s.idx", sha1[1]))
			jt.AddJob(fmt.Sprintf(".git/objects/pack/pack-%s.pack", sha1[1]))
		}
		concurrency := utils.MinInt(maxConcurrency, int(jt.QueuedJobs()))
		for w := 1; w <= concurrency; w++ {
			go workers.DownloadWorker(c, baseUrl, baseDir, jt, false, false)
		}
		jt.StartAndWait()
	}

	log.Info().Str("base", baseUrl).Msg("finding objects")
	objs := make(map[string]bool) // object "set"
	//var packed_objs [][]byte

	files := []string{
		utils.Url(baseDir, ".git/packed-refs"),
		utils.Url(baseDir, ".git/info/refs"),
		utils.Url(baseDir, ".git/FETCH_HEAD"),
		utils.Url(baseDir, ".git/ORIG_HEAD"),
		utils.Url(baseDir, ".git/HEAD"),
	}

	gitRefsDir := utils.Url(baseDir, ".git/refs")
	if utils.Exists(gitRefsDir) {
		if err := filepath.Walk(gitRefsDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				files = append(files, path)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	gitLogsDir := utils.Url(baseDir, ".git/logs")
	if utils.Exists(gitLogsDir) {
		refLogPrefix := utils.Url(gitLogsDir, "refs") + "/"
		if err := filepath.Walk(gitLogsDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				files = append(files, path)

				if strings.HasPrefix(path, refLogPrefix) {
					refName := strings.TrimPrefix(path, refLogPrefix)
					filePath := utils.Url(gitRefsDir, refName)
					if !utils.Exists(filePath) {
						log.Info().Str("dir", baseDir).Str("ref", refName).Msg("generating ref file")

						content, err := ioutil.ReadFile(path)
						if err != nil {
							log.Error().Str("dir", baseDir).Str("ref", refName).Err(err).Msg("couldn't read reflog file")
							return nil
						}

						// Find the last reflog entry and extract the obj hash and write that to the ref file
						logObjs := refLogRegex.FindAllSubmatch(content, -1)
						lastEntryObj := logObjs[len(logObjs)-1][1]

						if err := utils.CreateParentFolders(filePath); err != nil {
							log.Error().Str("file", filePath).Err(err).Msg("couldn't create parent directories")
							return nil
						}

						if err := ioutil.WriteFile(filePath, lastEntryObj, os.ModePerm); err != nil {
							log.Error().Str("file", filePath).Err(err).Msg("couldn't write to file")
						}
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, f := range files {
		if !utils.Exists(f) {
			continue
		}

		content, err := ioutil.ReadFile(f)
		if err != nil {
			log.Error().Str("file", f).Err(err).Msg("couldn't read reflog file")
			return err
		}

		for _, obj := range objRegex.FindAll(content, -1) {
			objs[strings.TrimSpace(string(obj))] = true
		}
	}

	indexPath := utils.Url(baseDir, ".git/index")
	if utils.Exists(indexPath) {
		f, err := os.Open(indexPath)
		if err != nil {
			return err
		}
		defer f.Close()
		var idx index.Index
		decoder := index.NewDecoder(f)
		if err := decoder.Decode(&idx); err != nil {
			log.Error().Str("dir", baseDir).Err(err).Msg("couldn't decode git index")
		}
		for _, entry := range idx.Entries {
			objs[entry.Hash.String()] = true
		}
	}

	storage := filesystem.NewObjectStorage(dotgit.New(osfs.New(utils.Url(baseDir, ".git"))), &cache.ObjectLRU{MaxSize: 256})
	if err := storage.ForEachObjectHash(func(hash plumbing.Hash) error {
		objs[hash.String()] = true
		encObj, err := storage.EncodedObject(plumbing.AnyObject, hash)
		if err != nil {
			return err

		}
		decObj, err := object.DecodeObject(storage, encObj)
		if err != nil {
			return err
		}
		for _, hash := range utils.GetReferencedHashes(decObj) {
			objs[hash] = true
		}
		return nil
	}); err != nil {
		log.Error().Str("dir", baseDir).Err(err).Msg("error while processing object files")
	}
	// TODO: find more objects to fetch in pack files and remove packed objects from list of objects to be fetched
	/*for _, pack := range storage.ObjectPacks() {
		storage.IterEncodedObjects()
	}*/

	jt = jobtracker.NewJobTracker()
	log.Info().Str("base", baseUrl).Msg("fetching object")
	for obj := range objs {
		jt.AddJob(obj)
	}
	for w := 1; w <= maxConcurrency; w++ {
		go workers.FindObjectsWorker(c, baseUrl, baseDir, jt, storage)
	}
	jt.StartAndWait()

	// TODO: does this even make sense???????
	if !utils.Exists(baseDir) {
		return nil
	}

	log.Info().Str("dir", baseDir).Msg("running git checkout .")
	cmd := exec.Command("git", "checkout", ".")
	cmd.Dir = baseDir
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if exErr, ok := err.(*exec.ExitError); ok && (exErr.ProcessState.ExitCode() == 255 || exErr.ProcessState.ExitCode() == 128) {
			log.Info().Str("base", baseUrl).Str("dir", baseDir).Msg("attempting to fetch missing files")
			out, err := ioutil.ReadAll(stderr)
			if err != nil {
				return err
			}
			var missingFiles []string
			errors := stdErrRegex.FindAllSubmatch(out, -1)
			jt = jobtracker.NewJobTracker()
			for _, e := range errors {
				if !bytes.HasSuffix(e[1], phpSuffix) {
					missingFiles = append(missingFiles, string(e[1]))
					jt.AddJob(string(e[1]))
				}
			}
			concurrency := utils.MinInt(maxConcurrency, int(jt.QueuedJobs()))
			for w := 1; w <= concurrency; w++ {
				go workers.DownloadWorker(c, baseUrl, baseDir, jt, true, true)
			}
			jt.StartAndWait()

			/*// Fetch files marked as missing in status
			// TODO: why do we parse status AND decode index ???????
			cmd := exec.Command("git", "status")
			cmd.Dir = baseDir
			stdout := &bytes.Buffer{}
			cmd.Stdout = stdout
			err = cmd.Run()
			// ignore errors, this will likely error almost every time
			if err == nil {
				out, err = ioutil.ReadAll(stdout)
				if err != nil {
					return err
				}
				deleted := statusRegex.FindAllSubmatch(out, -1)
				jt = jobtracker.NewJobTracker()
				for _, e := range deleted {
					if !bytes.HasSuffix(e[1], phpSuffix) {
						jt.AddJob(string(e[1]))
					}
				}
				concurrency = utils.MinInt(maxConcurrency, int(jt.QueuedJobs()))
				for w := 1; w <= concurrency; w++ {
					go workers.DownloadWorker(c, baseUrl, baseDir, jt, true, true)
				}
				jt.Wait()
			}*/

			// Iterate over index to find missing files
			var idx index.Index
			var hasIndex bool
			if utils.Exists(indexPath) {
				f, err := os.Open(indexPath)
				if err != nil {
					return err
				}
				defer f.Close()
				decoder := index.NewDecoder(f)
				if err := decoder.Decode(&idx); err != nil {
					fmt.Fprintf(os.Stderr, "error: %s\n", err)
					//return err
				} else {
					hasIndex = true
				}
				jt = jobtracker.NewJobTracker()
				for _, entry := range idx.Entries {
					if !strings.HasSuffix(entry.Name, ".php") && !utils.Exists(utils.Url(baseDir, entry.Name)) {
						missingFiles = append(missingFiles, entry.Name)
						jt.AddJob(entry.Name)
					}
				}
				concurrency = utils.MinInt(maxConcurrency, int(jt.QueuedJobs()))
				for w := 1; w <= concurrency; w++ {
					go workers.DownloadWorker(c, baseUrl, baseDir, jt, true, true)
				}
				jt.StartAndWait()
			}

			jt = jobtracker.NewJobTracker()
			for _, f := range missingFiles {
				if utils.Exists(utils.Url(baseDir, f)) {
					jt.AddJob(f)
				}
			}
			concurrency = utils.MinInt(maxConcurrency, int(jt.QueuedJobs()))
			var idp *index.Index
			if hasIndex {
				idp = &idx
			}
			for w := 1; w <= concurrency; w++ {
				go workers.CreateObjectWorker(baseDir, jt, storage, idp)
			}
			jt.StartAndWait()
		} else {
			return err
		}

		ignorePath := utils.Url(baseDir, ".gitignore")
		if utils.Exists(ignorePath) {
			log.Info().Str("base", baseDir).Msg("atempting to fetch ignored files")

			ignoreFile, err := os.Open(ignorePath)
			if err != nil {
				return err
			}
			defer ignoreFile.Close()

			jt = jobtracker.NewJobTracker()

			scanner := bufio.NewScanner(ignoreFile)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				commentStrip := strings.SplitN(line, "#", 1)
				line = commentStrip[0]
				if line == "" || strings.HasPrefix(line, "!") || strings.HasSuffix(line, "/") || strings.ContainsRune(line, '*') || strings.HasSuffix(line, ".php") {
					continue
				}
				jt.AddJob(line)
			}

			if err := scanner.Err(); err != nil {
				return err
			}

			concurrency = utils.MinInt(maxConcurrency, int(jt.QueuedJobs()))
			for w := 1; w <= concurrency; w++ {
				go workers.DownloadWorker(c, baseUrl, baseDir, jt, true, true)
			}
			jt.StartAndWait()
		}
	}
	return nil
}
