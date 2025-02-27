package workers

import (
	"fmt"
	"io/ioutil"
	"os"
	"sync"

	"github.com/deletescape/goop/internal/jobtracker"
	"github.com/deletescape/goop/internal/utils"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/phuslu/log"
	"github.com/valyala/fasthttp"
)

var checkedObjs = make(map[string]bool)
var checkedObjsMutex sync.Mutex

func FindObjectsWorker(c *fasthttp.Client, baseUrl, baseDir string, jt *jobtracker.JobTracker, storage *filesystem.ObjectStorage) {
	for {
		select {
		case obj := <-jt.Queue:
			findObjWork(c, baseUrl, baseDir, obj, jt, storage)
		default:
			if !jt.HasWork() {
				return
			}
			jt.Nap()
		}
	}
}

func findObjWork(c *fasthttp.Client, baseUrl, baseDir, obj string, jt *jobtracker.JobTracker, storage *filesystem.ObjectStorage) {
	jt.StartWork()
	defer jt.EndWork()

	checkRatelimted()

	if obj == "" {
		return
	}

	checkedObjsMutex.Lock()
	if checked, ok := checkedObjs[obj]; checked && ok {
		// Obj has already been checked
		checkedObjsMutex.Unlock()
		return
	} else {
		checkedObjs[obj] = true
	}
	checkedObjsMutex.Unlock()

	file := fmt.Sprintf(".git/objects/%s/%s", obj[:2], obj[2:])
	fullPath := utils.Url(baseDir, file)
	if utils.Exists(fullPath) {
		log.Info().Str("obj", obj).Msg("already fetched, skipping redownload")
		encObj, err := storage.EncodedObject(plumbing.AnyObject, plumbing.NewHash(obj))
		if err != nil {
			log.Error().Str("obj", obj).Err(err).Msg("couldn't read object")
			return
		}
		decObj, err := object.DecodeObject(storage, encObj)
		if err != nil {
			log.Error().Str("obj", obj).Err(err).Msg("couldn't decode object")
			return
		}
		referencedHashes := utils.GetReferencedHashes(decObj)
		for _, h := range referencedHashes {
			jt.AddJob(h)
		}
		return
	}

	uri := utils.Url(baseUrl, file)
	code, body, err := c.Get(nil, uri)
	if err == nil && code != 200 {
		if code == 429 {
			setRatelimited()
			jt.AddJob(obj)
			return
		}
		log.Warn().Str("obj", obj).Int("code", code).Msg("failed to fetch object")
		return
	} else if err != nil {
		log.Error().Str("obj", obj).Int("code", code).Err(err).Msg("failed to fetch object")
		return
	}

	if utils.IsHtml(body) {
		log.Warn().Str("uri", uri).Msg("file appears to be html, skipping")
		return
	}
	if utils.IsEmptyBytes(body) {
		log.Warn().Str("uri", uri).Msg("file appears to be empty, skipping")
		return
	}
	if err := utils.CreateParentFolders(fullPath); err != nil {
		log.Error().Str("uri", uri).Str("file", fullPath).Err(err).Msg("couldn't create parent directories")
		return
	}
	if err := ioutil.WriteFile(fullPath, body, os.ModePerm); err != nil {
		log.Error().Str("uri", uri).Str("file", fullPath).Err(err).Msg("clouldn't write file")
		return
	}

	log.Info().Str("obj", obj).Msg("fetched object")

	encObj, err := storage.EncodedObject(plumbing.AnyObject, plumbing.NewHash(obj))
	if err != nil {
		log.Error().Str("obj", obj).Err(err).Msg("couldn't read object")
		return
	}
	decObj, err := object.DecodeObject(storage, encObj)
	if err != nil {
		log.Error().Str("obj", obj).Err(err).Msg("couldn't decode object")
		return
	}
	referencedHashes := utils.GetReferencedHashes(decObj)
	for _, h := range referencedHashes {
		jt.AddJob(h)
	}
}
