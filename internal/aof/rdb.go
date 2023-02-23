package aof

import (
	"io/ioutil"
	"os"

	"slava/config"
	"slava/internal/interface/database"
	"slava/pkg/datastruct/dict"
	SortedSet "slava/pkg/datastruct/sortedset"
	"slava/pkg/logger"
	"slava/pkg/rdb/model"
	"strconv"
	"time"
)

// todo: forbid concurrent rewrite

// Rewrite2RDB rewrite aof data into rdb
func (persister *Persister) Rewrite2RDB(rdbFilename string) error {
	ctx, err := persister.startRewrite2RDB(nil, nil)
	if err != nil {
		return err
	}
	err = persister.rewrite2RDB(ctx)
	if err != nil {
		return err
	}
	err = ctx.tmpFile.Close()
	if err != nil {
		return err
	}
	err = os.Rename(ctx.tmpFile.Name(), rdbFilename)
	if err != nil {
		return err
	}
	return nil
}

// Rewrite2RDBForReplication asynchronously rewrite aof data into rdb and returns a channel to receive following data
// parameter listener would receive following updates of rdb
// parameter hook allows you to do something during aof pausing
func (persister Persister) Rewrite2RDBForReplication(rdbFilename string, listener aof.Listener, hook func()) error {
	ctx, err := persister.startRewrite2RDB(listener, hook)
	if err != nil {
		return err
	}
	err = persister.rewrite2RDB(ctx)
	if err != nil {
		return err
	}
	err = ctx.tmpFile.Close()
	if err != nil {
		return err
	}
	err = os.Rename(ctx.tmpFile.Name(), rdbFilename)
	if err != nil {
		return err
	}
	return nil
}

func (persister *Persister) startRewrite2RDB(newListener aof.Listener, hook func()) (*aof.RewriteCtx, error) {
	persister.pausingAof.Lock() // pausing aof
	defer persister.pausingAof.Unlock()

	err := persister.aofFile.Sync()
	if err != nil {
		logger.Warn("fsync failed")
		return nil, err
	}

	// get current aof file size
	fileInfo, _ := os.Stat(persister.aofFilename)
	filesize := fileInfo.Size()
	// create tmp file
	file, err := ioutil.TempFile("", "*.aof")
	if err != nil {
		logger.Warn("tmp file create failed")
		return nil, err
	}
	if newListener != nil {
		persister.listeners[newListener] = struct{}{}
	}
	if hook != nil {
		hook()
	}
	return &aof.RewriteCtx{
		tmpFile:  file,
		fileSize: filesize,
	}, nil
}

func (persister *Persister) rewrite2RDB(ctx *aof.RewriteCtx) error {
	// load aof tmpFile
	tmpHandler := persister.newRewriteHandler()
	tmpHandler.LoadAof(int(ctx.fileSize))
	encoder := rdb.NewEncoder(ctx.tmpFile).EnableCompress()
	err := encoder.WriteHeader()
	if err != nil {
		return err
	}
	auxMap := map[string]string{
		"redis-ver":    "6.0.0",
		"redis-bits":   "64",
		"aof-preamble": "0",
		"ctime":        strconv.FormatInt(time.Now().Unix(), 10),
	}
	for k, v := range auxMap {
		err := encoder.WriteAux(k, v)
		if err != nil {
			return err
		}
	}

	for i := 0; i < config.Properties.Databases; i++ {
		keyCount, ttlCount := tmpHandler.db.GetDBSize(i)
		if keyCount == 0 {
			continue
		}
		err = encoder.WriteDBHeader(uint(i), uint64(keyCount), uint64(ttlCount))
		if err != nil {
			return err
		}
		// dump db
		var err2 error
		tmpHandler.db.ForEach(i, func(key string, entity *database.DataEntity, expiration *time.Time) bool {
			var opts []interface{}
			if expiration != nil {
				opts = append(opts, rdb.WithTTL(uint64(expiration.UnixNano()/1e6)))
			}
			switch obj := entity.Data.(type) {
			case []byte:
				err = encoder.WriteStringObject(key, obj, opts...)
			case List.List:
				vals := make([][]byte, 0, obj.Len())
				obj.ForEach(func(i int, v interface{}) bool {
					bytes, _ := v.([]byte)
					vals = append(vals, bytes)
					return true
				})
				err = encoder.WriteListObject(key, vals, opts...)
			case *set.Set:
				vals := make([][]byte, 0, obj.Len())
				obj.ForEach(func(m string) bool {
					vals = append(vals, []byte(m))
					return true
				})
				err = encoder.WriteSetObject(key, vals, opts...)
			case dict.Dict:
				hash := make(map[string][]byte)
				obj.ForEach(func(key string, val interface{}) bool {
					bytes, _ := val.([]byte)
					hash[key] = bytes
					return true
				})
				err = encoder.WriteHashMapObject(key, hash, opts...)
			case *SortedSet.SortedSet:
				var entries []*model.ZSetEntry
				obj.ForEach(int64(0), obj.Len(), true, func(element *SortedSet.Element) bool {
					entries = append(entries, &model.ZSetEntry{
						Member: element.Member,
						Score:  element.Score,
					})
					return true
				})
				err = encoder.WriteZSetObject(key, entries, opts...)
			}
			if err != nil {
				err2 = err
				return false
			}
			return true
		})
		if err2 != nil {
			return err2
		}
	}
	err = encoder.WriteEnd()
	if err != nil {
		return err
	}
	return nil
}
