package server

import (
	"strings"
	"unicode"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"github.com/wsw/codex-gateway/internal/store"
)

// userSearchFields contains derived, non-authoritative fields used only by
// Owner-facing user pickers.  They are deliberately computed while building
// an HTTP response rather than persisted in the users table, so a display-name
// change never leaves a stale search index behind.
type userSearchFields struct {
	Pinyin         string `json:"pinyin"`
	PinyinFull     string `json:"pinyin_full"`
	PinyinInitials string `json:"pinyin_initials"`
	SearchIndex    string `json:"search_index"`
}

// The GBK ranges below are the conventional fallback used by Chinese input
// tools to determine a Han character's pinyin initial.  Full syllables are
// supplied for common names/labels below; for less common characters the
// initial remains searchable and the original character is retained in the
// index. This keeps the response-time index compact while still covering
// names typically used in the dashboard and test fixtures.
type gbkInitialRange struct {
	startFirst  byte
	startSecond byte
	endFirst    byte
	endSecond   byte
	letter      byte
}

var gbkInitialRanges = [...]gbkInitialRange{
	{0xB0, 0xA1, 0xB0, 0xC4, 'a'},
	{0xB0, 0xC5, 0xB2, 0xC0, 'b'},
	{0xB2, 0xC1, 0xB2, 0xC4, 'c'},
	{0xB2, 0xC5, 0xB2, 0xC7, 'd'},
	{0xB2, 0xC8, 0xB4, 0xA4, 'e'},
	{0xB4, 0xA5, 0xB4, 0xF9, 'f'},
	{0xB4, 0xFA, 0xB7, 0xA1, 'g'},
	{0xB7, 0xA2, 0xB8, 0xC0, 'h'},
	{0xB8, 0xC1, 0xB8, 0xC2, 'j'},
	{0xB8, 0xC3, 0xB9, 0xC4, 'k'},
	{0xB9, 0xC5, 0xBA, 0xF6, 'l'},
	{0xBA, 0xF7, 0xBF, 0xD5, 'm'},
	{0xBF, 0xD6, 0xC0, 0xA5, 'n'},
	{0xC0, 0xA6, 0xC0, 0xB9, 'o'},
	{0xC0, 0xBA, 0xC2, 0xD9, 'p'},
	{0xC2, 0xDA, 0xC4, 0xD6, 'q'},
	{0xC4, 0xD7, 0xC5, 0xF2, 'r'},
	{0xC5, 0xF3, 0xC7, 0xBA, 's'},
	{0xC7, 0xBB, 0xC8, 0xF5, 't'},
	{0xC8, 0xF6, 0xCB, 0xF9, 'w'},
	{0xCB, 0xFA, 0xCD, 0xD9, 'x'},
	{0xCD, 0xDA, 0xCE, 0xF3, 'y'},
	{0xCE, 0xF4, 0xD1, 0xB9, 'z'},
}

// commonPinyin intentionally contains no user data; it is a compact fallback
// dictionary for ordinary Chinese names and labels.  Unknown characters are
// still indexed by their original rune and GBK initial.
var commonPinyin = map[rune]string{
	'阿': "a", '安': "an", '爱': "ai", '艾': "ai", '白': "bai", '班': "ban", '宝': "bao", '保': "bao",
	'北': "bei", '本': "ben", '比': "bi", '边': "bian", '彬': "bin", '冰': "bing", '博': "bo", '才': "cai",
	'彩': "cai", '曹': "cao", '曾': "zeng", '岑': "cen", '查': "zha", '昌': "chang", '超': "chao", '陈': "chen",
	'晨': "chen", '成': "cheng", '程': "cheng", '诚': "cheng", '池': "chi", '楚': "chu", '川': "chuan", '春': "chun",
	'崔': "cui", '达': "da", '戴': "dai", '丹': "dan", '旦': "dan", '德': "de", '迪': "di", '丁': "ding",
	'东': "dong", '董': "dong", '杜': "du", '段': "duan", '朵': "duo", '恩': "en", '方': "fang", '芳': "fang",
	'飞': "fei", '菲': "fei", '芬': "fen", '峰': "feng", '凤': "feng", '福': "fu", '傅': "fu", '甘': "gan",
	'刚': "gang", '高': "gao", '歌': "ge", '葛': "ge", '根': "gen", '光': "guang", '桂': "gui", '国': "guo",
	'海': "hai", '韩': "han", '涵': "han", '航': "hang", '豪': "hao", '浩': "hao", '何': "he", '贺': "he",
	'河': "he", '红': "hong", '宏': "hong", '侯': "hou", '胡': "hu", '华': "hua", '欢': "huan", '黄': "huang",
	'辉': "hui", '惠': "hui", '慧': "hui", '吉': "ji", '佳': "jia", '家': "jia", '坚': "jian", '江': "jiang",
	'杰': "jie", '金': "jin", '晶': "jing", '静': "jing", '君': "jun", '俊': "jun", '凯': "kai", '康': "kang",
	'可': "ke", '柯': "ke", '科': "ke", '克': "ke", '孔': "kong", '宽': "kuan", '兰': "lan", '岚': "lan",
	'乐': "le", '雷': "lei", '黎': "li", '丽': "li", '莉': "li", '利': "li", '李': "li",
	'力': "li", '林': "lin", '琳': "lin", '玲': "ling", '凌': "ling", '龙': "long", '露': "lu", '陆': "lu",
	'路': "lu", '吕': "lv", '绿': "lv", '马': "ma", '梅': "mei", '美': "mei", '孟': "meng", '梦': "meng",
	'明': "ming", '敏': "min", '娜': "na", '南': "nan", '宁': "ning", '鹏': "peng", '平': "ping", '强': "qiang",
	'倩': "qian", '琴': "qin", '青': "qing", '秋': "qiu", '全': "quan", '然': "ran", '仁': "ren", '荣': "rong",
	'瑞': "rui", '若': "ruo", '三': "san", '森': "sen", '山': "shan", '杉': "shan", '尚': "shang", '少': "shao",
	'申': "shen", '沈': "shen", '生': "sheng", '胜': "sheng", '诗': "shi", '石': "shi", '时': "shi", '世': "shi",
	'书': "shu", '舒': "shu", '帅': "shuai", '顺': "shun", '思': "si", '丝': "si", '松': "song", '苏': "su",
	'孙': "sun", '四': "si", '涛': "tao", '天': "tian", '田': "tian", '甜': "tian", '婷': "ting", '彤': "tong",
	'同': "tong", '童': "tong", '万': "wan", '王': "wang", '伟': "wei", '文': "wen", '温': "wen", '武': "wu",
	'希': "xi", '曦': "xi", '霞': "xia", '贤': "xian", '香': "xiang", '向': "xiang", '小': "xiao", '肖': "xiao",
	'晓': "xiao", '欣': "xin", '新': "xin", '鑫': "xin", '兴': "xing", '雄': "xiong", '秀': "xiu", '徐': "xu",
	'雪': "xue", '雅': "ya", '亚': "ya", '燕': "yan", '阳': "yang", '杨': "yang", '洋': "yang", '瑶': "yao",
	'叶': "ye", '一': "yi", '怡': "yi", '依': "yi", '义': "yi", '英': "ying", '勇': "yong", '宇': "yu",
	'玉': "yu", '瑜': "yu", '雨': "yu", '元': "yuan", '源': "yuan", '远': "yuan", '月': "yue", '云': "yun",
	'泽': "ze", '张': "zhang", '章': "zhang", '哲': "zhe", '真': "zhen", '珍': "zhen", '正': "zheng", '志': "zhi",
	'智': "zhi", '中': "zhong", '舟': "zhou", '周': "zhou", '朱': "zhu", '竹': "zhu", '子': "zi", '紫': "zi",
	'宗': "zong", '祖': "zu", '左': "zuo", '赵': "zhao", '郑': "zheng", '钟': "zhong", '重': "zhong", '卓': "zhuo",
	'五': "wu", '六': "liu", '七': "qi", '八': "ba", '九': "jiu", '零': "ling",
}

func deriveUserSearchFields(username, displayName string) userSearchFields {
	full := pinyinForText(displayName)
	initials := pinyinInitialsForText(displayName)
	username = strings.ToLower(strings.TrimSpace(username))
	displayName = strings.ToLower(strings.TrimSpace(displayName))
	search := strings.Join([]string{username, displayName, full, initials}, " ")
	return userSearchFields{Pinyin: full, PinyinFull: full, PinyinInitials: initials, SearchIndex: search}
}

func matchesDerivedUserSearch(id, username, displayName, query string) bool {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return true
	}
	fields := deriveUserSearchFields(username, displayName)
	index := strings.ToLower(strings.Join([]string{id, fields.SearchIndex}, " "))
	for _, term := range terms {
		if !strings.Contains(index, term) {
			return false
		}
	}
	return true
}

func pinyinForText(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if syllable, ok := commonPinyin[character]; ok {
			builder.WriteString(syllable)
			continue
		}
		if unicode.IsSpace(character) {
			continue
		}
		if character < 128 {
			builder.WriteRune(unicode.ToLower(character))
			continue
		}
		builder.WriteRune(character)
	}
	return strings.ReplaceAll(builder.String(), "ü", "v")
}

func pinyinInitialsForText(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if syllable, ok := commonPinyin[character]; ok && syllable != "" {
			builder.WriteByte(syllable[0])
			continue
		}
		if character < 128 {
			if unicode.IsLetter(character) || unicode.IsDigit(character) {
				builder.WriteRune(unicode.ToLower(character))
			}
			continue
		}
		if initial, ok := gbkInitial(character); ok {
			builder.WriteByte(initial)
		} else {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func gbkInitial(character rune) (byte, bool) {
	encoded, _, err := transform.String(simplifiedchinese.GBK.NewEncoder(), string(character))
	if err != nil {
		return 0, false
	}
	bytes := []byte(encoded)
	if len(bytes) != 2 {
		return 0, false
	}
	for _, value := range gbkInitialRanges {
		atOrAfterStart := bytes[0] > value.startFirst || (bytes[0] == value.startFirst && bytes[1] >= value.startSecond)
		atOrBeforeEnd := bytes[0] < value.endFirst || (bytes[0] == value.endFirst && bytes[1] <= value.endSecond)
		if atOrAfterStart && atOrBeforeEnd {
			return value.letter, true
		}
	}
	return 0, false
}

func addUserSearchFields(target map[string]any, username, displayName string) {
	fields := deriveUserSearchFields(username, displayName)
	target["pinyin"] = fields.Pinyin
	target["pinyin_full"] = fields.PinyinFull
	target["pinyin_initials"] = fields.PinyinInitials
	target["search_index"] = fields.SearchIndex
}

func modelAccessUsersResponse(users []store.ModelAccessUser) []map[string]any {
	items := make([]map[string]any, 0, len(users))
	for _, user := range users {
		item := map[string]any{
			"model": user.Model, "user_id": user.UserID, "username": user.Username,
			"display_name": user.DisplayName, "role": user.Role, "status": user.Status,
			"enabled": user.Enabled, "updated_at": user.UpdatedAt,
		}
		if user.UpdatedByUserID != nil {
			item["updated_by_user_id"] = user.UpdatedByUserID
		}
		addUserSearchFields(item, user.Username, user.DisplayName)
		items = append(items, item)
	}
	return items
}
