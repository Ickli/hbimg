package main
// TODO: safeMode. all printlns may be altered for safe mode
// TODO: safeMode. replace all std errors with custom ones

import (
	"io"
	"fmt"
	"sync"
	"bytes"
	"io/fs"
	"errors"
	"slices"
	"strings"
	"strconv"
	fp "path/filepath"
	"os"
	"net/url"
	"net/http"
	"encoding/base64"
	"image"
	rg "regexp"
	_ "image/jpeg"
	_ "image/png"
	_ "image/gif"
)

type pathEntry struct {
	path string;
	entry fs.DirEntry;
}

type hbimgError struct {
	base error;
	filePath string;
	tagIndex int;
}

type argInfo struct {
	desc string;
	f func(string) int;
}

type IdValuePair struct {
	id []byte;
	value []byte;
}

func (e hbimgError) Error() string {
	return "ERR PLACEHOLDER";
}

const (
	GO_COUNT_DEFAULT = 4;
	LIST_DEFAULT_CAP = 16;
	ID_DEFAULT_CAP = 48;
	BUF_DEFAULT_CAP = 2*1048576;
	IMGBUF_DEFAULT_CAP = 1048576;
	
	INTS_PER_TAG_NAME = 2;
	INTS_PER_TAG_ATTR = 4;
	ATTRS_PER_IMG = 3
	INTS_PER_IMG = INTS_PER_TAG_NAME + ATTRS_PER_IMG*INTS_PER_TAG_ATTR;
	TAG_SRC, TAG_ID, TAG_NO_HBIMG = "src", "id", "data-no-hbimg";
)
var ARGS map[string]argInfo;

// matches <img> with or without leftmost 'src', 'id' and 'data-no-hbimg' attributes and its values captured
// in different capture groups, so they can be used by '...Submatch' functions;
// captures at most 3 such tags separated by any character including newline.
// TODO: try find regex shorter, which still captures
var rgImg = rg.MustCompile(`(?s)<.*?img(?:[^>]*?(src|data-no-hbimg|id)\s*=\s*"(.*?)")?(?:[^>]*?(src|data-no-hbimg|id)\s*=\s*"(.*?)")?(?:[^>]*?(src|data-no-hbimg|id)\s*=\s*"(.*?)")?.*?>`);
var rgCloseHTML = rg.MustCompile("</html>");

var safeMode = false;
var moveToScript = false;
var goCount = GO_COUNT_DEFAULT;
var pairSlices [][]IdValuePair;
var errorSlices [][] error;
var bufs [][]byte;
var outDir = "hbimg_output";
var outDirAbs string;
var CWD string;
var preserveDirStructure = false;
var filesPassedViaArgs = false;
var justShowHelp = false;

func main() {
	initOptions();

	var errCWD error;
	CWD, errCWD = fp.Abs(".");
	if errCWD != nil {
		fmt.Println(errCWD.Error());
		os.Exit(1);
	}
	outDirAbs = getFullPath(CWD, outDir);

	var fsv = os.DirFS(".");
	var htmls = make([]pathEntry, 0, LIST_DEFAULT_CAP);
	var errParse = parseArgs(&htmls);

	if errParse != nil {
		fmt.Println( errParse.Error());
		os.Exit(1);
	}
	if justShowHelp {
		return;
	}

	errorSlices = init2D[error](goCount, LIST_DEFAULT_CAP);
	if moveToScript {
		pairSlices = init2D[IdValuePair](goCount, LIST_DEFAULT_CAP);
	}

	if !filesPassedViaArgs {
		fs.WalkDir(fsv, ".", func(path string, d fs.DirEntry, err error) error {
			return appendIfOutsideOutDir(getFullPath(CWD, path), d, err, &htmls);
		});
	}

	bufs = init2D[byte](min(len(htmls), goCount), BUF_DEFAULT_CAP);
	var htmlsProcessed = 0;
	var htmlsTotal = len(htmls);
	var routines sync.WaitGroup;

	for;htmlsProcessed < htmlsTotal; {
		for i := 0; i < goCount; i++ {
			if htmlsProcessed == htmlsTotal {
				goto out;
			}

			routines.Add(1);
			go handleHTML(htmls[htmlsProcessed].path, i, &routines);
			htmlsProcessed++;
		}
		routines.Wait();
		handleErrors(goCount);
	}
	out:
	routines.Wait();
	handleErrors(goCount);
}

func handleHTML(curFile string, routineId int, wg *sync.WaitGroup) {
	defer wg.Done();
	defer clearBuf(routineId);

	var outputPath = getAbsPathFitStructure(curFile);
	var outputDir, _ = fp.Split(outputPath);
	var curDir, _ = fp.Split(curFile);
	var inbuf, err = os.ReadFile(curFile);

	if err != nil {
		errorSlices[routineId] = append(errorSlices[routineId], err);
		return;
	}

	err = os.MkdirAll(outputDir, 0777);

	if err != nil {
		errorSlices[routineId] = append(errorSlices[routineId], err);
		return;
	}
	
	translateHTML(inbuf, curDir, routineId);

	err = os.WriteFile(outputPath, bufs[routineId], 0777);

	if err != nil {
		errorSlices[routineId] = append(errorSlices[routineId], err);
	}
}

func translateHTML(inbuf []byte, curDir string, routineId int) {
	var matches = rgImg.FindAllSubmatchIndex(inbuf, -1);
	var outbuf = &bufs[routineId];
	var curStart = 0;

	for _, matchList := range matches {
		// match, src, value, id, value. Doesn't support tag without value
		*outbuf = append(*outbuf, inbuf[curStart:matchList[0]]...);
		var id, basedValue, err = translateImgTag(inbuf, outbuf, matchList, curDir);
		if err != nil {
			errorSlices[routineId] = append(errorSlices[routineId], err);
		} else if moveToScript {
			pairSlices[routineId] = append(pairSlices[routineId], IdValuePair{id: id, value: basedValue});
		}

		curStart = matchList[1];
	}

	if curStart > len(inbuf) {
		return;
	}

	var closeHTMLIndex = -1
	var closeHTMLPair = rgCloseHTML.FindIndex(inbuf[curStart:]);

	if closeHTMLPair != nil {
		closeHTMLIndex = curStart + closeHTMLPair[0];
		*outbuf = append(*outbuf, inbuf[curStart:closeHTMLIndex]...);
	}

	if moveToScript {
		writeScript(outbuf, pairSlices[routineId]);
	}
	
	if closeHTMLPair != nil {
		*outbuf = append(*outbuf, []byte("</html>")...);
	}
}

func clearBuf(rId int) {
	bufs[rId] = bufs[rId][:1];
}

// if moveToScript set to true and no error happens,
// returns (id of translated tag, nil, nil) and writes to outbuf;
// otherwise, returns id of translated tag, translated img, nil
func translateImgTag(inbuf []byte, outbuf *[]byte, matchList []int, curDir string) ([]byte, []byte, error) {
	var matchLen = min(len(matchList), INTS_PER_IMG);
	var curStart = matchList[0];
	var id, translated []byte;
	var err error = nil;
	var toTranslate = true;
	var toGenerateId = false;

	for i := INTS_PER_TAG_NAME; i < matchLen && matchList[i] != -1; i += INTS_PER_TAG_ATTR {
		var tag = inbuf[matchList[i]:matchList[i+1]];
		var value = inbuf[matchList[i+2]:matchList[i+3]];

		if string(tag) == TAG_NO_HBIMG && string(value) == "true" {
			toTranslate = false;
			break;
		}
	}
	
	for i := INTS_PER_TAG_NAME; i < matchLen && matchList[i] != -1; i += INTS_PER_TAG_ATTR {
		var tag = inbuf[matchList[i]:matchList[i+1]];
		var value = inbuf[matchList[i+2]:matchList[i+3]];

		*outbuf = append(*outbuf, inbuf[curStart:matchList[i]]...);

		var tagStr = string(tag);
		if tagStr == TAG_SRC {
			if !toTranslate {
				*outbuf = append(*outbuf, []byte(" src=\"")...);
				*outbuf = append(*outbuf, value...);
				*outbuf = append(*outbuf, '"');
			} else if moveToScript {
				translated = make([]byte, 0, IMGBUF_DEFAULT_CAP);
				err = translateSrc(&translated, value, curDir);
			} else {
				*outbuf = append(*outbuf, []byte(" src=\"")...);
				err = translateSrc(outbuf, value, curDir);
				*outbuf = append(*outbuf, '"');
			}
		} else if tagStr == TAG_ID {
			toGenerateId = false;
			id = value;
			*outbuf = append(*outbuf, []byte(" id=\"")...);
			*outbuf = append(*outbuf, id...);
			*outbuf = append(*outbuf, '"');
		} else {
			*outbuf = append(*outbuf, tag...);
			*outbuf = append(*outbuf, []byte("=\"")...);
			*outbuf = append(*outbuf, value...);
			*outbuf = append(*outbuf, '"');
		}

		curStart = matchList[i+3] + 1; // + 1 because '"' already written
	}

	if toGenerateId {
		id = make([]byte, 0, ID_DEFAULT_CAP);
		id = append(id, []byte("hbimg_auto_")...);
		id = append(id, []byte(strconv.Itoa(matchList[0]))...);
		*outbuf = append(*outbuf, []byte(" id=\"")...);
		*outbuf = append(*outbuf, id...);
		*outbuf = append(*outbuf, '"');
	}

	*outbuf = append(*outbuf, inbuf[curStart:matchList[1]]...);

	return id, translated, err;
}

// handles both URL and filepath
func translateSrc(outbuf *[]byte, srcBytes []byte, curDir string) error {
	var handleError = func(err error) error {
		*outbuf = append(*outbuf, srcBytes...);
		return err;
	}

	var srcStr = string(srcBytes);
	var _, errUrl = url.ParseRequestURI(srcStr);
	var imgbuf []byte;
	var errImg error;

	if errUrl == nil {
		imgbuf, errUrl = getImgBytesFromURL(srcStr);
		if errUrl != nil {
			return handleError(errUrl);
		}
	} else if fs.ValidPath(srcStr) {
		imgbuf, errImg = getImgBytesFromImg(getFullPath(curDir, srcStr));
		if errImg != nil {
			return handleError(errImg);
		}
	} else {
		return handleError(errors.New("'src' tag contains neither URL nor file path"));
	}

	var encodeBuf = make([]byte, 0);
	encodeBuf = base64.StdEncoding.AppendEncode(encodeBuf, imgbuf);
	var _, format, errDecode = image.DecodeConfig(bytes.NewReader(imgbuf));

	if errDecode != nil {
		return handleError(errDecode);
	}

	*outbuf = append(*outbuf, []byte("data:image/")...);
	*outbuf = append(*outbuf, []byte(format)...);
	*outbuf = append(*outbuf, []byte(";base64,")...);
	*outbuf = append(*outbuf, encodeBuf...);

	return nil;
}

func getImgBytesFromURL(url string) ([]byte, error) {
	var resp, errGet = http.Get(url);

	if errGet != nil {
		return []byte{}, errors.New("couldn't get response from url");
	}

	var imgbuf, errRead = io.ReadAll(resp.Body);
	_ = resp.Body.Close();

	return imgbuf, errRead;
}

func getImgBytesFromImg(path string) ([]byte, error) {
	var buf []byte;
	var err error = nil;
	var fileInfo os.FileInfo;
	var file *os.File;

	file, err = os.Open(path);
	if err != nil {
		return buf, err;
	}
	
	fileInfo, err = file.Stat();
	if err != nil {
		return buf, err;
	}

	buf = make([]byte, fileInfo.Size());
	_, err = file.Read(buf);
	if err != nil {
		return buf, err;
	}

	err = file.Close();
	return buf, err;
}

 func writeScript(outbuf *[]byte, idValuePairs []IdValuePair) {
	*outbuf = append(*outbuf, []byte("\n<script>\n")...);
	for _, pair := range idValuePairs {
		*outbuf = append(*outbuf, []byte("document.getElementById(\"")...);
		*outbuf = append(*outbuf, pair.id...);
		*outbuf = append(*outbuf, []byte("\").src=\"")...);
		*outbuf = append(*outbuf, pair.value...);
		*outbuf = append(*outbuf, []byte("\";\n")...);
	}
	*outbuf = append(*outbuf, []byte("</script>\n")...);
 }

// returns targpath if is absolute,
// otherwise returns joined basepath and targpath
func getFullPath(basepath, targpath string) string {
	if fp.IsAbs(targpath) {
		return targpath;
	}
	return fp.Join(basepath, targpath);
}

func getAbsPathFitStructure(path string) string {
	var err error;
	if fp.IsAbs(path) {
		path, err = fp.Rel(CWD, path);
		if err != nil {
			fmt.Println(err.Error());
			os.Exit(1);
		}
	}
	for; strings.HasPrefix(path, "../"); {
		path = path[3:];
	}
	return fp.Join(outDirAbs, path);
}

func init2D[T any](flen, slen int) [][]T {
	var slice = make([][]T, flen);
	for i := range slice {
		slice[i] = make([]T, 0, slen);
	}
	return slice;
}

// TODO: handle them for safeMode!
func handleErrors(goCount int) {
	for i := 0; i < goCount; i++ {
		var errors = errorSlices[i];
		if len(errors) == 0 {
			continue;
		}
		for j := 0; j < len(errors); j++ {
			fmt.Println(errors[j].Error());
		}
		errorSlices[i] = errorSlices[i][:1];
	}
}

func parseArgs(htmls *[]pathEntry) error {
	var args = os.Args;
	var argsLen = len(args);
	var narg string;
	
	for i := 1; i < argsLen; i++ {
		if i < argsLen - 1 {
			narg = args[i+1];
		} else {
			narg = "";
		}
		
		var arg = args[i];
		var option, ok = ARGS[arg];
		if ok {
			i += option.f(narg);
		} else if arg[0] != '-' {
			var stat, err = os.Stat(arg);
			if err != nil {
				return errors.New("Couldn't access '" + arg + "' file/dir");
			}

			filesPassedViaArgs = true;
			if !stat.IsDir() {
				*htmls = append(*htmls, pathEntry{path: arg, entry: fs.FileInfoToDirEntry(stat)});
				continue;
			}

			var fsv = os.DirFS(arg);
			fs.WalkDir(fsv, ".", func(path string, d os.DirEntry, err error) error {
				return appendIfOutsideOutDir(getFullPath(CWD, getFullPath(arg, path)), d, err, htmls);
			});

		} else {
			ARGS["--help"].f("");
			return errors.New("There's no '" + arg + "' flag");
		}
	}

	return nil;
}

func appendIfOutsideOutDir(absPath string, d os.DirEntry, err error, slice *[]pathEntry) error {
	var isRel = strings.HasPrefix(absPath, outDirAbs);

	if !isRel {
		return appendIfHTML(absPath, d, err, slice);
	}
	return nil;
}


func appendIfHTML(path string, d os.DirEntry, err error, slice *[]pathEntry) error {
	if err != nil {
		return err;
	}
	var ext = fp.Ext(d.Name());
	if ext == ".html" || ext == ".htm" {
		*slice = append(*slice, pathEntry{path: path, entry: d});
	}
	return nil;
} 

func setOutDir(relPath string) {
	outDir = relPath;
	outDirAbs = getFullPath(CWD, relPath);
}

func initOptions() {
	ARGS = map[string]argInfo{
		"--help": {desc: "Show help", f: func (_ string) int {
			justShowHelp = true;
			
			var keys = make([]string, 0, LIST_DEFAULT_CAP);
			for key, _ := range ARGS {
				keys = append(keys, key);
			}
			slices.Sort(keys);

			for _, key := range keys {
				fmt.Println(key, " ", ARGS[key].desc);
			}
			return 0;
		}},
		/*
		"-s": {desc: "Don't show full paths of files", f: func(_ string) int { 
			safeMode = true;
			return 0;
		}},
		*/
		"-c": {desc: "-c <count>: Specify count of goroutines running", f: func(next string) int {
			if goCount == 0 {
				return 1;
			}
	
			var err error;
			goCount, err = strconv.Atoi(next);
			if err != nil {
				return 1;
			}
			if goCount <= 0 {
				goCount = GO_COUNT_DEFAULT;
			}

			return 1;
		}},
		"-j": {desc: "Move all base64 info in '<script>' tag", f: func(_ string) int { 
			moveToScript = true;
			return 0;
		}},
		"-o": {desc: "Specify output directory", f: func(next string) int {
			setOutDir(next);
			return 1;
		}},
	};
}
