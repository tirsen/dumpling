package export

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/pingcap/br/pkg/storage"

	"github.com/pingcap/dumpling/v4/log"
)

const lengthLimit = 1048576

// TODO make this configurable, 5 mb is a good minimum size but on low latency/high bandwidth network you can go a lot bigger
const hardcodedS3ChunkSize = 5 * 1024 * 1024

var pool = sync.Pool{New: func() interface{} {
	return &bytes.Buffer{}
}}

type writerPipe struct {
	input  chan *bytes.Buffer
	closed chan struct{}
	errCh  chan error

	currentFileSize      uint64
	currentStatementSize uint64

	fileSizeLimit      uint64
	statementSizeLimit uint64

	w io.Writer
}

func newWriterPipe(w io.Writer, fileSizeLimit, statementSizeLimit uint64) *writerPipe {
	return &writerPipe{
		input:  make(chan *bytes.Buffer, 8),
		closed: make(chan struct{}),
		errCh:  make(chan error, 1),
		w:      w,

		currentFileSize:      0,
		currentStatementSize: 0,
		fileSizeLimit:        fileSizeLimit,
		statementSizeLimit:   statementSizeLimit,
	}
}

func (b *writerPipe) Run(ctx context.Context) {
	defer close(b.closed)
	var errOccurs bool
	for {
		select {
		case s, ok := <-b.input:
			if !ok {
				return
			}
			if errOccurs {
				continue
			}
			err := writeBytes(b.w, s.Bytes())
			s.Reset()
			pool.Put(s)
			if err != nil {
				errOccurs = true
				b.errCh <- err
			}
		case <-ctx.Done():
			return
		}
	}
}

func (b *writerPipe) AddFileSize(fileSize uint64) {
	b.currentFileSize += fileSize
	b.currentStatementSize += fileSize
}

func (b *writerPipe) Error() error {
	select {
	case err := <-b.errCh:
		return err
	default:
		return nil
	}
}

func (b *writerPipe) ShouldSwitchFile() bool {
	return b.fileSizeLimit != UnspecifiedSize && b.currentFileSize >= b.fileSizeLimit
}

func (b *writerPipe) ShouldSwitchStatement() bool {
	return (b.fileSizeLimit != UnspecifiedSize && b.currentFileSize >= b.fileSizeLimit) ||
		(b.statementSizeLimit != UnspecifiedSize && b.currentStatementSize >= b.statementSizeLimit)
}

func WriteMeta(meta MetaIR, w io.Writer) error {
	log.Debug("start dumping meta data", zap.String("target", meta.TargetName()))

	specCmtIter := meta.SpecialComments()
	for specCmtIter.HasNext() {
		if err := write(w, fmt.Sprintf("%s\n", specCmtIter.Next())); err != nil {
			return err
		}
	}

	if err := write(w, meta.MetaSQL()); err != nil {
		return err
	}

	log.Debug("finish dumping meta data", zap.String("target", meta.TargetName()))
	return nil
}

func WriteInsert(pCtx context.Context, tblIR TableDataIR, w io.Writer, fileSizeLimit, statementSizeLimit uint64) error {
	fileRowIter := tblIR.Rows()
	if !fileRowIter.HasNext() {
		return nil
	}

	bf := pool.Get().(*bytes.Buffer)
	if bfCap := bf.Cap(); bfCap < lengthLimit {
		bf.Grow(lengthLimit - bfCap)
	}

	wp := newWriterPipe(w, fileSizeLimit, statementSizeLimit)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wp.Run(ctx)
		wg.Done()
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	specCmtIter := tblIR.SpecialComments()
	for specCmtIter.HasNext() {
		bf.WriteString(specCmtIter.Next())
		bf.WriteByte('\n')
	}
	wp.currentFileSize += uint64(bf.Len())

	var (
		insertStatementPrefix string
		row                   = MakeRowReceiver(tblIR.ColumnTypes())
		counter               = 0
		escapeBackSlash       = tblIR.EscapeBackSlash()
		err                   error
	)

	selectedField := tblIR.SelectedField()
	// if has generated column
	if selectedField != "" {
		insertStatementPrefix = fmt.Sprintf("INSERT INTO %s %s VALUES\n",
			wrapBackTicks(escapeString(tblIR.TableName())), selectedField)
	} else {
		insertStatementPrefix = fmt.Sprintf("INSERT INTO %s VALUES\n",
			wrapBackTicks(escapeString(tblIR.TableName())))
	}
	insertStatementPrefixLen := uint64(len(insertStatementPrefix))

	for fileRowIter.HasNext() {
		wp.currentStatementSize = 0
		bf.WriteString(insertStatementPrefix)
		wp.AddFileSize(insertStatementPrefixLen)

		for fileRowIter.HasNext() {
			if err = fileRowIter.Decode(row); err != nil {
				log.Error("scanning from sql.Row failed", zap.Error(err))
				return err
			}

			lastBfSize := bf.Len()
			row.WriteToBuffer(bf, escapeBackSlash)
			counter += 1
			wp.AddFileSize(uint64(bf.Len()-lastBfSize) + 2) // 2 is for ",\n" and ";\n"

			fileRowIter.Next()
			shouldSwitch := wp.ShouldSwitchStatement()
			if fileRowIter.HasNext() && !shouldSwitch {
				bf.WriteString(",\n")
			} else {
				bf.WriteString(";\n")
			}
			if bf.Len() >= lengthLimit {
				wp.input <- bf
				bf = pool.Get().(*bytes.Buffer)
				if bfCap := bf.Cap(); bfCap < lengthLimit {
					bf.Grow(lengthLimit - bfCap)
				}
			}

			select {
			case <-pCtx.Done():
				return pCtx.Err()
			case err := <-wp.errCh:
				return err
			default:
			}

			if shouldSwitch {
				break
			}
		}
		if wp.ShouldSwitchFile() {
			break
		}
	}
	log.Debug("dumping table",
		zap.String("table", tblIR.TableName()),
		zap.Int("record counts", counter))
	if bf.Len() > 0 {
		wp.input <- bf
	}
	close(wp.input)
	<-wp.closed
	if err = fileRowIter.Error(); err != nil {
		return err
	}
	return wp.Error()
}

func WriteInsertInCsv(pCtx context.Context, tblIR TableDataIR, w io.Writer, noHeader bool, opt *csvOption, fileSizeLimit uint64) error {
	fileRowIter := tblIR.Rows()
	if !fileRowIter.HasNext() {
		return nil
	}

	bf := pool.Get().(*bytes.Buffer)
	if bfCap := bf.Cap(); bfCap < lengthLimit {
		bf.Grow(lengthLimit - bfCap)
	}

	wp := newWriterPipe(w, fileSizeLimit, UnspecifiedSize)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wp.Run(ctx)
		wg.Done()
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	var (
		row             = MakeRowReceiver(tblIR.ColumnTypes())
		counter         = 0
		escapeBackSlash = tblIR.EscapeBackSlash()
		err             error
	)

	if !noHeader && len(tblIR.ColumnNames()) != 0 {
		for i, col := range tblIR.ColumnNames() {
			bf.Write(opt.delimiter)
			escape([]byte(col), bf, getEscapeQuotation(escapeBackSlash, opt.delimiter))
			bf.Write(opt.delimiter)
			if i != len(tblIR.ColumnTypes())-1 {
				bf.Write(opt.separator)
			}
		}
		bf.WriteByte('\n')
	}
	wp.currentFileSize += uint64(bf.Len())

	for fileRowIter.HasNext() {
		if err = fileRowIter.Decode(row); err != nil {
			log.Error("scanning from sql.Row failed", zap.Error(err))
			return err
		}

		lastBfSize := bf.Len()
		row.WriteToBufferInCsv(bf, escapeBackSlash, opt)
		counter += 1
		wp.currentFileSize += uint64(bf.Len()-lastBfSize) + 1 // 1 is for "\n"

		bf.WriteByte('\n')
		if bf.Len() >= lengthLimit {
			wp.input <- bf
			bf = pool.Get().(*bytes.Buffer)
			if bfCap := bf.Cap(); bfCap < lengthLimit {
				bf.Grow(lengthLimit - bfCap)
			}
		}

		fileRowIter.Next()
		select {
		case <-pCtx.Done():
			return pCtx.Err()
		case err := <-wp.errCh:
			return err
		default:
		}
		if wp.ShouldSwitchFile() {
			break
		}
	}

	log.Debug("dumping table",
		zap.String("table", tblIR.TableName()),
		zap.Int("record counts", counter))
	if bf.Len() > 0 {
		wp.input <- bf
	}
	close(wp.input)
	<-wp.closed
	if err = fileRowIter.Error(); err != nil {
		return err
	}
	return wp.Error()
}

func write(writer io.Writer, str string) error {
	_, err := writer.Write([]byte(str))
	if err != nil {
		// str might be very long, only output the first 200 chars
		outputLength := len(str)
		if outputLength >= 200 {
			outputLength = 200
		}
		log.Error("writing failed",
			zap.String("string", str[:outputLength]),
			zap.Error(err))
	}
	return err
}

func writeBytes(writer io.Writer, p []byte) error {
	_, err := writer.Write(p)
	if err != nil {
		// str might be very long, only output the first 200 chars
		outputLength := len(p)
		if outputLength >= 200 {
			outputLength = 200
		}
		log.Error("writing failed",
			zap.ByteString("string", p[:outputLength]),
			zap.String("writer", fmt.Sprintf("%#v", writer)),
			zap.Error(err))
	}
	return err
}

func buildFileWriter(ctx context.Context, s storage.ExternalStorage, path string) (io.Writer, func(), error) {
	fullPath := s.URI() + path
	uploader, err := s.CreateUploader(ctx, path)
	if err != nil {
		log.Error("open file failed",
			zap.String("path", fullPath),
			zap.Error(err))
		return nil, nil, err
	}
	writer := storage.NewUploaderWriter(ctx, uploader, hardcodedS3ChunkSize)
	log.Debug("opened file", zap.String("path", fullPath))
	tearDownRoutine := func() {
		err := writer.Close()
		if err == nil {
			return
		}
		log.Error("close file failed",
			zap.String("path", fullPath),
			zap.Error(err))
	}
	return writer, tearDownRoutine, nil
}

func buildInterceptFileWriter(ctx context.Context, s storage.ExternalStorage, path string) (io.WriteCloser, func()) {
	var writer io.WriteCloser
	fullPath := s.URI() + path
	fileWriter := &InterceptFileWriter{ctx: ctx}
	initRoutine := func(ctx context.Context) error {
		uploader, err := s.CreateUploader(ctx, path)
		if err != nil {
			log.Error("open file failed",
				zap.String("path", fullPath),
				zap.Error(err))
			return err
		}
		w := storage.NewUploaderWriter(ctx, uploader, hardcodedS3ChunkSize)
		writer = w
		log.Debug("opened file", zap.String("path", fullPath))
		fileWriter.WriteCloser = writer
		return err
	}
	fileWriter.initRoutine = initRoutine

	tearDownRoutine := func() {
		if writer == nil {
			return
		}
		log.Debug("tear down lazy file writer...")
		err := writer.Close()
		if err != nil {
			log.Error("close file failed", zap.String("path", fullPath))
		}
	}
	return fileWriter, tearDownRoutine
}

type LazyStringWriter struct {
	initRoutine func() error
	sync.Once
	io.StringWriter
	err error
}

func (l *LazyStringWriter) WriteString(str string) (int, error) {
	l.Do(func() { l.err = l.initRoutine() })
	if l.err != nil {
		return 0, fmt.Errorf("open file error: %s", l.err.Error())
	}
	return l.StringWriter.WriteString(str)
}

// InterceptFileWriter is an interceptor of os.File,
// tracking whether a StringWriter has written something.
type InterceptFileWriter struct {
	io.WriteCloser
	sync.Once
	ctx         context.Context
	initRoutine func(context.Context) error
	err         error

	SomethingIsWritten bool
}

func (w *InterceptFileWriter) Write(p []byte) (int, error) {
	w.Do(func() { w.err = w.initRoutine(w.ctx) })
	if len(p) > 0 {
		w.SomethingIsWritten = true
	}
	if w.err != nil {
		return 0, fmt.Errorf("open file error: %s", w.err.Error())
	}
	return w.WriteCloser.Write(p)
}

func (w *InterceptFileWriter) Close() error {
	return w.WriteCloser.Close()
}

func wrapBackTicks(identifier string) string {
	if !strings.HasPrefix(identifier, "`") && !strings.HasSuffix(identifier, "`") {
		return wrapStringWith(identifier, "`")
	}
	return identifier
}

func wrapStringWith(str string, wrapper string) string {
	return fmt.Sprintf("%s%s%s", wrapper, str, wrapper)
}
