package content

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jfrog/gofrog/http/retryexecutor"
	"github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"io"
	"os"
	"reflect"
	"sort"
	"sync"
)

// Open and read JSON files, find the array key inside it and load its value into the memory in small chunks.
// Currently, 'ContentReader' only support extracting a single value for a given key (arrayKey), other keys are ignored.
// The value must be of type array.
// Each array value can be fetched using 'NextRecord' (thread-safe).
// This technique solves the limit of memory size which may be too small to fit large JSON.
type ContentReader struct {
	// filesPaths - source data file paths.
	filesPaths []string
	// arrayKey - Read the value of the specific object in JSON.
	arrayKey string
	// The objects from the source data file are being pushed into the data channel.
	dataChannel chan map[string]interface{}
	errorsQueue *utils.ErrorsQueue
	once        *sync.Once
	// Number of elements in the array (cache)
	length int
	empty  bool
}

func NewContentReader(filePath string, arrayKey string) *ContentReader {
	self := NewMultiSourceContentReader([]string{filePath}, arrayKey)
	self.empty = filePath == ""
	return self
}

func NewMultiSourceContentReader(filePaths []string, arrayKey string) *ContentReader {
	self := ContentReader{}
	self.filesPaths = filePaths
	self.arrayKey = arrayKey
	self.dataChannel = make(chan map[string]interface{}, utils.MaxBufferSize)
	self.errorsQueue = utils.NewErrorsQueue(utils.MaxBufferSize)
	self.once = new(sync.Once)
	self.empty = len(filePaths) == 0
	return &self
}

func NewEmptyContentReader(arrayKey string) *ContentReader {
	self := NewContentReader("", arrayKey)
	return self
}

func (cr *ContentReader) IsEmpty() bool {
	return cr.empty
}

// Each call to 'NextRecord()' will return a single element from the channel.
// Only the first call invokes a goroutine to read data from the file and push it into the channel.
// 'io.EOF' will be returned if no data is left.
func (cr *ContentReader) NextRecord(recordOutput interface{}) error {
	if cr.empty {
		return errorutils.CheckErrorf("Empty")
	}
	cr.once.Do(func() {
		go func() {
			defer close(cr.dataChannel)
			cr.length = 0
			cr.run()
		}()
	})
	record, ok := <-cr.dataChannel
	if !ok {
		return io.EOF
	}
	// Transform the data into a Go type
	err := ConvertToStruct(record, &recordOutput)
	if err != nil {
		cr.errorsQueue.AddError(err)
		return err
	}
	cr.length++
	return err
}

// Prepare the reader to read the file all over again (not thread-safe).
func (cr *ContentReader) Reset() {
	cr.dataChannel = make(chan map[string]interface{}, utils.MaxBufferSize)
	cr.once = new(sync.Once)
}

func removeFileWithRetry(filePath string) error {
	// Check if file exists before attempting to remove
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		log.Debug("File does not exist: %s", filePath)
		return nil
	}
	log.Debug("Attempting to remove file: %s", filePath)
	executor := retryexecutor.RetryExecutor{
		Context:                  context.Background(),
		MaxRetries:               5,
		RetriesIntervalMilliSecs: 100,
		ErrorMessage:             "Failed to remove file",
		LogMsgPrefix:             "Attempting removal",
		ExecutionHandler: func() (bool, error) {
			return false, errorutils.CheckError(os.Remove(filePath))
		},
	}
	return executor.Execute()
}

// Cleanup the reader data with retry
func (cr *ContentReader) Close() error {
	for _, filePath := range cr.filesPaths {
		if filePath == "" {
			continue
		}
		if err := removeFileWithRetry(filePath); err != nil {
			return fmt.Errorf("failed to close reader: %w", err)
		}
	}
	cr.filesPaths = nil
	return nil
}

func (cr *ContentReader) GetFilesPaths() []string {
	return cr.filesPaths
}

// Number of element in the array.
func (cr *ContentReader) Length() (int, error) {
	if cr.empty {
		return 0, nil
	}
	if cr.length == 0 {
		for item := new(interface{}); cr.NextRecord(item) == nil; item = new(interface{}) {
		}
		cr.Reset()
		if err := cr.GetError(); err != nil {
			return 0, err
		}
	}
	return cr.length, nil
}

// Open and read the files one by one. Push each array element into the channel.
// The channel may block the thread, therefore should run async.
func (cr *ContentReader) run() {
	for _, filePath := range cr.filesPaths {
		cr.readSingleFile(filePath)
	}
}

func (cr *ContentReader) readSingleFile(filePath string) {
	fd, err := os.Open(filePath)
	if err != nil {
		log.Error(err.Error())
		cr.errorsQueue.AddError(errorutils.CheckError(err))
		return
	}
	defer func() {
		err = fd.Close()
		if err != nil {
			log.Error(err.Error())
			cr.errorsQueue.AddError(errorutils.CheckError(err))
		}
	}()
	br := bufio.NewReaderSize(fd, 65536)
	dec := json.NewDecoder(br)
	err = findDecoderTargetPosition(dec, cr.arrayKey, true)
	if err != nil {
		if err == io.EOF {
			cr.errorsQueue.AddError(errorutils.CheckErrorf(cr.arrayKey + " not found"))
			return
		}
		cr.errorsQueue.AddError(err)
		log.Error(err.Error())
		return
	}
	for dec.More() {
		var ResultItem map[string]interface{}
		err = dec.Decode(&ResultItem)
		if err != nil {
			log.Error(err)
			cr.errorsQueue.AddError(errorutils.CheckError(err))
			return
		}
		cr.dataChannel <- ResultItem
	}
}

func (cr *ContentReader) GetError() error {
	return cr.errorsQueue.GetError()
}

// Search and set the decoder's position at the desired key in the JSON file.
// If the desired key is not found, return io.EOF
func findDecoderTargetPosition(dec *json.Decoder, target string, isArray bool) error {
	for dec.More() {
		// Token returns the next JSON token in the input stream.
		t, err := dec.Token()
		if err != nil {
			return errorutils.CheckError(err)
		}
		if t == target {
			if isArray {
				// Skip '['
				_, err = dec.Token()
			}
			return errorutils.CheckError(err)
		}
	}
	return nil
}

func MergeReaders(arr []*ContentReader, arrayKey string) (contentReader *ContentReader, err error) {
	cw, err := NewContentWriter(arrayKey, true, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, cw.Close())
	}()
	for _, cr := range arr {
		for item := new(interface{}); cr.NextRecord(item) == nil; item = new(interface{}) {
			cw.Write(*item)
		}
		if err = cr.GetError(); err != nil {
			return nil, err
		}
	}
	contentReader = NewContentReader(cw.GetFilePath(), arrayKey)
	return contentReader, nil
}

// Sort a content-reader in the required order (ascending or descending).
// Performs a merge-sort on the reader, splitting the reader to multiple readers of size 'utils.MaxBufferSize'.
// Sort each of the split readers, and merge them into a single sorted reader.
// In case of multiple items with the same key - all the items will appear in the sorted reader, but their order is not guaranteed to be preserved.
func SortContentReader(readerRecord SortableContentItem, reader *ContentReader, ascendingOrder bool) (*ContentReader, error) {
	getSortKeyFunc := func(record interface{}) (string, error) {
		// Get the expected record type from the reader.
		recordType := reflect.ValueOf(readerRecord).Type()
		recordItem := (reflect.New(recordType)).Interface()
		err := ConvertToStruct(record, &recordItem)
		if err != nil {
			return "", err
		}
		contentItem, ok := recordItem.(SortableContentItem)
		if !ok {
			return "", errorutils.CheckErrorf("attempting to sort a content-reader with unsortable items")
		}
		return contentItem.GetSortKey(), nil
	}
	return SortContentReaderByCalculatedKey(reader, getSortKeyFunc, ascendingOrder)
}

type keyCalculationFunc func(interface{}) (string, error)

type SortRecord struct {
	Key    string      `json:"key,omitempty"`
	Record interface{} `json:"record,omitempty"`
}

func (sr SortRecord) GetSortKey() string {
	return sr.Key
}

// Sort a ContentReader, according to a key generated by getKeyFunc.
// getKeyFunc gets an item from the reader and returns the key of the item.
// Attention! In case of multiple items with the same key - only the first item in the original reader will appear in the sorted one! The other items will be removed.
// Also pay attention that the order of the fields inside the objects might change.
func SortContentReaderByCalculatedKey(reader *ContentReader, getKeyFunc keyCalculationFunc, ascendingOrder bool) (contentReader *ContentReader, err error) {
	var sortedReaders []*ContentReader
	defer func() {
		for _, r := range sortedReaders {
			err = errors.Join(err, r.Close())
		}
	}()

	// Split reader to multiple sorted readers of size 'utils.MaxBufferSize'.
	sortedReaders, err = splitReaderToSortedBufferSizeReadersByCalculatedKey(reader, getKeyFunc, ascendingOrder)
	if err != nil {
		return nil, err
	}

	// Merge the sorted readers.
	return mergeSortedReadersByCalculatedKey(sortedReaders, ascendingOrder)
}

// Split the reader to multiple readers of size 'utils.MaxBufferSize' to prevent memory overflow.
// Sort each split-reader content according to the provided 'ascendingOrder'.
func splitReaderToSortedBufferSizeReadersByCalculatedKey(reader *ContentReader, getKeyFunc keyCalculationFunc, ascendingOrder bool) ([]*ContentReader, error) {
	var splitReaders []*ContentReader

	// Split and sort.
	keysToContentItems := make(map[string]SortableContentItem)
	allKeys := make([]string, 0, utils.MaxBufferSize)
	for newRecord := new(interface{}); reader.NextRecord(newRecord) == nil; newRecord = new(interface{}) {
		sortKey, err := getKeyFunc(newRecord)
		if err != nil {
			return nil, err
		}

		if _, exist := keysToContentItems[sortKey]; !exist {
			recordWrapper := &SortRecord{Key: sortKey, Record: newRecord}
			keysToContentItems[sortKey] = recordWrapper
			allKeys = append(allKeys, sortKey)
			if len(allKeys) == utils.MaxBufferSize {
				sortedFile, err := SortAndSaveBufferToFile(keysToContentItems, allKeys, ascendingOrder)
				if err != nil {
					return nil, err
				}
				splitReaders = append(splitReaders, sortedFile)
				keysToContentItems = make(map[string]SortableContentItem)
				allKeys = make([]string, 0, utils.MaxBufferSize)
			}
		}
	}
	if err := reader.GetError(); err != nil {
		return nil, err
	}
	reader.Reset()
	if len(allKeys) > 0 {
		sortedFile, err := SortAndSaveBufferToFile(keysToContentItems, allKeys, ascendingOrder)
		if err != nil {
			return nil, err
		}
		splitReaders = append(splitReaders, sortedFile)
	}

	return splitReaders, nil
}

func mergeSortedReadersByCalculatedKey(sortedReaders []*ContentReader, ascendingOrder bool) (contentReader *ContentReader, err error) {
	if len(sortedReaders) == 0 {
		contentReader = NewEmptyContentReader(DefaultKey)
		return contentReader, nil
	}
	resultWriter, err := NewContentWriter(DefaultKey, true, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, resultWriter.Close())
	}()
	currentContentItem := make([]*SortRecord, len(sortedReaders))
	sortedFilesClone := make([]*ContentReader, len(sortedReaders))
	copy(sortedFilesClone, sortedReaders)

	for {
		var candidateToWrite *SortRecord
		smallestIndex := 0
		for i := 0; i < len(sortedFilesClone); i++ {
			if currentContentItem[i] == nil && sortedFilesClone[i] != nil {
				record := new(SortRecord)
				if err := sortedFilesClone[i].NextRecord(record); nil != err {
					sortedFilesClone[i] = nil
					continue
				}
				currentContentItem[i] = record
			}

			var candidateKey, currentKey string
			if candidateToWrite != nil && currentContentItem[i] != nil {
				candidateKey = candidateToWrite.Key
				currentKey = currentContentItem[i].Key

				// If there are two items with the same key - the second one will be removed
				if candidateKey == currentKey {
					currentContentItem[i] = nil
				}
			}
			if candidateToWrite == nil || (currentContentItem[i] != nil && compareStrings(candidateKey, currentKey, ascendingOrder)) {
				candidateToWrite = currentContentItem[i]
				smallestIndex = i
			}
		}
		if candidateToWrite == nil {
			break
		}
		resultWriter.Write(candidateToWrite.Record)
		currentContentItem[smallestIndex] = nil
	}
	contentReader = NewContentReader(resultWriter.GetFilePath(), resultWriter.GetArrayKey())
	return contentReader, nil
}

// Merge a slice of sorted content-readers into a single sorted content-reader.
func MergeSortedReaders(readerRecord SortableContentItem, sortedReaders []*ContentReader, ascendingOrder bool) (contentReader *ContentReader, err error) {
	if len(sortedReaders) == 0 {
		return NewEmptyContentReader(DefaultKey), nil
	}
	resultWriter, err := NewContentWriter(DefaultKey, true, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, resultWriter.Close())
	}()

	// Get the expected record type from the reader.
	value := reflect.ValueOf(readerRecord)
	valueType := value.Type()

	currentContentItem := make([]*SortableContentItem, len(sortedReaders))
	sortedFilesClone := make([]*ContentReader, len(sortedReaders))
	copy(sortedFilesClone, sortedReaders)

	for {
		var candidateToWrite *SortableContentItem
		smallestIndex := 0
		for i := 0; i < len(sortedFilesClone); i++ {
			if currentContentItem[i] == nil && sortedFilesClone[i] != nil {
				temp := (reflect.New(valueType)).Interface()
				if err = sortedFilesClone[i].NextRecord(temp); nil != err {
					sortedFilesClone[i] = nil
					continue
				}
				// Expect to receive 'SortableContentItem'.
				contentItem, ok := (temp).(SortableContentItem)
				if !ok {
					return nil, errorutils.CheckErrorf("attempting to sort a content-reader with unsortable items.")
				}
				currentContentItem[i] = &contentItem
			}

			if candidateToWrite == nil || (currentContentItem[i] != nil && compareStrings((*candidateToWrite).GetSortKey(),
				(*currentContentItem[i]).GetSortKey(), ascendingOrder)) {
				candidateToWrite = currentContentItem[i]
				smallestIndex = i
			}
		}
		if candidateToWrite == nil {
			break
		}
		resultWriter.Write(*candidateToWrite)
		currentContentItem[smallestIndex] = nil
	}
	contentReader = NewContentReader(resultWriter.GetFilePath(), resultWriter.GetArrayKey())
	return contentReader, nil
}

func compareStrings(src, against string, ascendingOrder bool) bool {
	if ascendingOrder {
		return src > against
	}
	return src < against
}

func SortAndSaveBufferToFile(keysToContentItems map[string]SortableContentItem, allKeys []string, increasingOrder bool) (contentReader *ContentReader, err error) {
	if len(allKeys) == 0 {
		return nil, nil
	}
	writer, err := NewContentWriter(DefaultKey, true, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, writer.Close())
	}()
	if increasingOrder {
		sort.Strings(allKeys)
	} else {
		sort.Sort(sort.Reverse(sort.StringSlice(allKeys)))
	}
	for _, v := range allKeys {
		writer.Write(keysToContentItems[v])
	}
	contentReader = NewContentReader(writer.GetFilePath(), writer.GetArrayKey())
	return contentReader, nil
}

func ConvertToStruct(record, recordOutput interface{}) error {
	data, err := json.Marshal(record)
	if errorutils.CheckError(err) != nil {
		return err
	}
	err = errorutils.CheckError(json.Unmarshal(data, recordOutput))
	return err
}
