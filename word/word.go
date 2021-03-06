package word

import (
	"bytes"
	"encoding/json"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/smartwalle/dbs"
	"github.com/smartwalle/ini4go"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"time"
)

const (
	MaxIdleConns        = 100
	MaxIdleConnsPerHost = 100
)

var (
	httpClient  *http.Client
	Logger      *log.Logger
	key         string
	db          dbs.DB
	projectPath = path.Join(os.Getenv("HOME"), "Documents", "Kanna")
	configPath  = path.Join(projectPath, "config")
	speechPath  = path.Join(projectPath, "speech")
)

func init() {
	// 初始化log文件
	logFile, err := os.OpenFile(path.Join(projectPath, "kanna.log"), os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		Logger.Fatal(err)
	}
	Logger = log.New(logFile, "[word_translator] ", log.Ltime|log.Ldate|log.Lshortfile)

	// 读取config
	var config = ini4go.New(false)
	config.SetUniqueOption(true)
	config.Load(configPath)

	// 生成文件及目录
	_, err = os.Stat(projectPath)
	if err != nil {
		err := os.Mkdir(projectPath, os.ModeDir|0777)
		if err != nil {
			Logger.Fatal(err, projectPath)
		}
		_, err = os.Stat(speechPath)
		if err != nil {
			os.Mkdir(speechPath, os.ModeDir|0777)
		}
	}

	// 读取key
	key = config.GetValue("youdao", "key")

	// 初始化mysql
	db, err = dbs.NewSQL(config.GetValue("sql", "driver"),
		config.GetValue("sql", "url"),
		config.MustInt("sql", "max_open", 10),
		config.MustInt("sql", "max_idle", 5))
	if err != nil {
		panic(err)
	}

	// 初始化client
	httpClient = createHTTPClient()

}

func getMySQLSession() dbs.DB {
	return db
}

// ———————————————————————————————————————————— Handler -----------------------------------------------------

var queryWordChan = make(chan string, 10)
var wordListChan = make(chan string, 10)
var writer io.Writer

func queryWordEnter(word string) {
	if word == "" {
		return
	}
	w, err := queryWord(word)
	if err != nil {
		Logger.Println(err)
	} else {
		w.FormatTranslations()
	}
}

func (this *wordHandler) RegisterFlag() (flagDict map[string]*chan string) {
	flagDict = make(map[string]*chan string)
	flagDict["w"] = &queryWordChan
	flagDict["wl"] = &wordListChan
	go this.run()
	return
}

func (this *wordHandler) DemandWriter(w io.Writer) {
	writer = w
}

type wordHandler struct{}

func NewWordHandler() *wordHandler {
	return new(wordHandler)
}

func (this *wordHandler) run() {
	for {
		select {
		case word := <-queryWordChan:
			queryWordEnter(word)
		case n := <-wordListChan:
			wordListEnter(n)
		}
	}
}

// ———————————————————————————————————————————— Service -----------------------------------------------------

type word struct {
	Id           int64      `json:"id"                sql:"id"`
	Word         string     `json:"word"              sql:"word"`
	Translations string     `json:"translations"      sql:"translations"`
	CreatedOn    *time.Time `json:"created_on"        sql:"created_on"`
	AppearTime   int        `json:"appear_time"       sql:"appear_time"`
	LastAppear   *time.Time `json:"last_appear"       sql:"last_appear"`
	EnglishTrans string     `json:"english_trans"     sql:"english_trans"`
}

func (w *word) FormatTranslations() {
	var fi interface{}
	json.Unmarshal([]byte(w.Translations), &fi)
	f := fi.(map[string]interface{})
	fmt.Fprintln(writer, "---- ", w.Word, " ----")
	if v, ok := f["translation"]; ok {
		fmt.Fprintln(writer, "基本翻译: ", v.([]interface{}))
	}
	if w.EnglishTrans != "" {
		fmt.Fprintln(writer, "英英: ", w.EnglishTrans)
	}
	if v, ok := f["basic"]; ok {
		basic := v.(map[string]interface{})
		if v, ok := basic["us-phonetic"]; ok {
			fmt.Fprintln(writer, "美式发音: ", v.(string))
		}
		if v, ok := basic["uk-phonetic"]; ok {
			fmt.Fprintln(writer, "英式发音: ", v.(string))
		}
		if v, ok := basic["explains"]; ok {
			fmt.Fprintln(writer, "其他释义: ", v.([]interface{}))
		}
		if v, ok := basic["us-speech"]; ok {
			f, err := os.Open(fmt.Sprint(speechPath+"/", w.Word, ".mp3"))
			if err == nil {
				go playMP3(f.Name())
			}
			f.Close()
			usURL := fmt.Sprint(v.(string))
			go downloadMP3(w.Word, usURL)
		}
	}
}

func (w *word) FormatWordList(writer io.Writer) {
	var fi interface{}
	json.Unmarshal([]byte(w.Translations), &fi)
	f := fi.(map[string]interface{})
	fmt.Fprint(writer, "---- ", w.Word, " -- ")
	if v, ok := f["translation"]; ok {
		fmt.Fprintln(writer, v.([]interface{})[0], "----")
	}
	if v, ok := f["basic"]; ok {
		basic := v.(map[string]interface{})
		if v, ok := basic["explains"]; ok {
			fmt.Fprintln(writer, "其他释义: ", v.([]interface{}))
		}
	}
}

func createHTTPClient() *http.Client {
	client := &http.Client{
		Timeout: time.Second * 10,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        MaxIdleConns,
			MaxIdleConnsPerHost: MaxIdleConnsPerHost,
			IdleConnTimeout:     time.Second * 90,
		},
	}
	return client
}

func youDaoTranslate(searchWord string) (result *word, err error) {
	requestUrl := fmt.Sprintf("http://fanyi.youdao.com/openapi.do?keyfrom=YouDaoCV&key=%s&type=data&doctype=json&version=1.2&q=%s", key, searchWord)
	req, err := http.NewRequest(http.MethodGet, requestUrl, nil)

	resp, err := httpClient.Do(req)
	defer resp.Body.Close()
	if err != nil {
		Logger.Println("获取api出错", err)
	}

	if resp.Status != "200 OK" {
		Logger.Println("调用翻译api发生错误")
		return nil, err
	}

	var wordStruct = new(word)
	wordStruct.Word = searchWord
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	wordStruct.Translations = buf.String()

	return wordStruct, nil
}

func downloadMP3(name, url string) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		Logger.Println(" download MP3 err :", err)
		return
	}

	// 查看是否有存放语音的文件夹

	f, err := os.OpenFile(fmt.Sprint(speechPath, "/", name, ".mp3"), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	defer f.Close()
	if err != nil {
		Logger.Fatal(err)
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		Logger.Println(" download MP3 err :", err)
		return
	}
	io.Copy(f, resp.Body)
	resp.Body.Close()
	playMP3(f.Name())
}

func playMP3(path string) {
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("afplay", path)
		//cmd.Start()
		// todo diff
		cmd.Run()
	}
}

func queryWord(searchWord string) (result *word, err error) {
	word, sqlErr := sqlGetWord(searchWord)
	if sqlErr == nil {
		go sqlUpdateWord(word.Id)
		return word, nil
	}

	result, err = youDaoTranslate(searchWord)
	result.EnglishTrans = sqlGetE2ETranslation(searchWord)
	if err != nil {
		return nil, err
	}
	err = sqlAddWord(result)
	if err != nil {
		Logger.Println(err)
	}
	return result, nil
}

// ---------------------- word list service --------------------------

func wordListEnter(sn string) {
	n, err := strconv.Atoi(sn)
	if err != nil {
		n = 5
	}
	pushWordList(n)
}

// 生成单词列表
func pushWordList(n int) {
	wordList, err := SqlCreateWordList(n)
	if err != nil {
		return
	}
	for n, word := range wordList {
		fmt.Fprint(writer, n+1, " ")
		word.FormatWordList(writer)
		fmt.Fprintln(writer, "--------------------------------------")
	}
}

// ———————————————————————————————————————————— MySQL -----------------------------------------------------

func sqlAddWord(word *word) (err error) {
	db := getMySQLSession()
	stmt, err := db.Prepare(`INSERT INTO notebook_word (word, translations, created_on, appear_time, last_appear) 
				VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		Logger.Println(err)
		return err
	}
	now := time.Now()
	_, err = stmt.Exec(word.Word, word.Translations, now, 1, now)
	if err != nil {
		Logger.Println(err)
		return err
	}
	return nil
}

func sqlGetWord(qWord string) (result *word, err error) {
	result = new(word)
	db := getMySQLSession()
	stmt, err := db.Prepare(`SELECT nw.id, nw.word, nw.translations, ee.translation AS english_trans
							FROM notebook_word AS nw Left Join english_to_english_dictionary AS ee ON ee.word = nw.word WHERE nw.word = ?`)
	row := stmt.QueryRow(qWord)
	err = row.Scan(&result.Id, &result.Word, &result.Translations, &result.EnglishTrans)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func sqlGetE2ETranslation(word string) (result string) {
	db := getMySQLSession()
	stmt, err := db.Prepare(`SELECT  ee.translation AS english_trans 
							FROM  english_to_english_dictionary AS ee where ee.word = ?`)
	row := stmt.QueryRow(word)
	err = row.Scan(&result)
	if err != nil {
		return ""
	}
	return result
}

func sqlUpdateWord(id int64) (err error) {
	db := getMySQLSession()
	stmt, err := db.Prepare(`UPDATE notebook_word
				SET appear_time = appear_time + 1, 
				last_appear = ?
				WHERE id = ?`)
	_, err = stmt.Exec(time.Now(), id)
	if err != nil {
		Logger.Println(err)
		return err
	}
	return
}
