package launchx

// 占位 SQL 的裸灌守卫。
//
// 背景：examples/mall/scripts/schema.sql 里应用账号的口令写成 shell 占位
// `${HUBX_DB_PW:-hubx123}`，而**重定向不经过 shell 展开** —— 把它直接 `<` 给
// mysql 客户端，那一句会原样执行：
//
//	CREATE USER 'mall_rw'@'%' IDENTIFIED BY '${HUBX_DB_PW:-hubx123}';
//
// 后果是最难查的那一类：账号建成功、GRANT 正常发出、SHOW GRANTS 也好看，
// 唯一症状是应用侧带着「每天都在用的那个口令」来连得到 ERROR 1045；而 1045
// 只说「访问被拒」，不提灌库姿势。本机 2026-10-14 因此把排查方向带去了网络、
// host 通配符和 MySQL 8 的认证插件，最后才发现库里的口令是那串字面量。
//
// 为什么这里用文本扫而不是 AST：判据的对象是**脚本与文档里的命令行形态**，
// 不是 Go 代码 —— 没有 AST 可走。于是注释就成了唯一噪声源（本仓已因「命中
// 注释」反复吃过亏，见 examples_addr_hygiene_test.go 的头注）。所以每条命中
// 都先跳过注释行；跳过之后仍然命中，就是真有人在命令里裸灌。
//
// 为什么同时扫 .md：这里的坏形态是**被人手从文档里抄走的**。脚本那一份已经被
// 收口到 examples/scripts/schema.sh，但文档里的命令行是给人复制的 —— 只要有一
// 份 README 还写着 `docker exec -i mysql ... < schema.sql`，下一个人就会照抄，
// 然后回到上面那条排查老路上。管住脚本不管文档，等于只关了一半门。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hasSchemaPlaceholder 报告这个文件是否真的用着 shell 占位（注释里的不算）。
func schemaPlaceholderLines(path string) ([]int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hits []int
	for i, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "--") || strings.HasPrefix(t, "#") {
			continue // SQL 注释：schema.sql 的头注会引用占位形态说明它是坏的
		}
		if strings.Contains(t, "${") && strings.Contains(t, "}") {
			hits = append(hits, i+1)
		}
	}
	return hits, nil
}

// isShellOrDocComment 按扩展名判断该行是不是注释。
//
// .md 里以 `#` 开头既可能是标题也可能是 bash 代码块里的 shell 注释，两种都
// 不该被当成可执行命令来判。
func isShellOrDocComment(trimmed string) bool {
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "--")
}

// mysqlRawFeedLines 找出「把 sqlFile 用 `<` 直接喂给 mysql 客户端」的行。
//
// 匹配两个形态：字面路径（`... < examples/mall/scripts/schema.sql`）与变量引用
// （`mysql_exec <"$SCHEMA_SQL"`，后者正是本次出事的那一条写法 —— 它看起来没有
// 硬编码路径，于是没人意识到它也没做展开）。
func mysqlRawFeedLines(content, sqlFile string) []int {
	base := filepath.Base(sqlFile)
	stem := strings.TrimSuffix(base, ".sql")
	upperStem := strings.ToUpper(stem)

	var hits []int
	for i, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if isShellOrDocComment(t) {
			continue
		}
		if !strings.Contains(t, "mysql") || !strings.Contains(t, "<") {
			continue
		}
		looks := strings.Contains(t, base) ||
			strings.Contains(strings.ToUpper(t), upperStem+"_SQL")
		if looks {
			hits = append(hits, i+1)
		}
	}
	return hits
}

func TestSchemaPlaceholderSQLIsNeverFedRawToMySQL(t *testing.T) {
	// 第一拍：先把「带占位的 SQL」找出来。判据不能假设文件名 —— 新增一份占位
	// SQL 却没登记，是最容易漏的场景，靠遍历自动发现它。
	placeholderSQLs := map[string][]int{}
	err := filepath.WalkDir("examples", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".sql" {
			return nil
		}
		lines, rerr := schemaPlaceholderLines(path)
		if rerr != nil {
			t.Errorf("读 %s 失败：%v", path, rerr)
			return nil
		}
		if len(lines) > 0 {
			placeholderSQLs[filepath.ToSlash(path)] = lines
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples 失败：%v（守卫失效）", err)
	}
	if len(placeholderSQLs) == 0 {
		t.Fatal("examples 下没有找到任何含 shell 占位的 .sql —— 若占位确实已全部移除，" +
			"请连同本守卫一起删掉；留着一条恒零命中的判据比没有更危险（它会把真违规一起淹没）")
	}

	// 第二拍：任何脚本/文档都不得把上面这些 SQL 直接重定向给 mysql。
	var violations []string
	scanned := 0
	err = filepath.WalkDir("examples", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".sh" && ext != ".md" {
			return nil
		}
		scanned++
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Errorf("读 %s 失败：%v", path, rerr)
			return nil
		}
		content := string(b)
		for sqlFile := range placeholderSQLs {
			for _, ln := range mysqlRawFeedLines(content, sqlFile) {
				violations = append(violations,
					filepath.ToSlash(path)+":"+itoa(ln)+
						"  把占位 SQL "+filepath.Base(sqlFile)+" 直接重定向给了 mysql —— "+
						"口令占位不会被展开，会变成字面量口令（ERROR 1045）。改走 bash examples/scripts/schema.sh apply")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples 失败：%v（守卫失效）", err)
	}
	if scanned < 10 {
		t.Fatalf("只扫到 %d 个 .sh/.md 文件，疑似路径基准错误（测试 cwd 应为仓根）", scanned)
	}
	if len(violations) > 0 {
		t.Fatalf("发现 %d 处占位 SQL 裸灌（共扫 %d 个脚本/文档）：\n  %s",
			len(violations), scanned, strings.Join(violations, "\n  "))
	}
	t.Logf("占位 SQL 裸灌守卫通过：占位 SQL %d 份，扫过 %d 个脚本/文档",
		len(placeholderSQLs), scanned)
}

// TestSchemaPlaceholderGuardCatchesRawFeed 是上面那条守卫的反向探针。
//
// 一条判据只有在「把坏形态喂给它、它确实报错」之后才算成立 —— 否则它就是个
// 恒绿的装饰品。这里把四种形态直接喂给同一个判据函数：两种必须被抓住，
// 两种必须放过去（放过 = 不产生假报，假报同样会毁掉这条判据的可信度）。
func TestSchemaPlaceholderGuardCatchesRawFeed(t *testing.T) {
	const sqlFile = "examples/mall/scripts/schema.sql"

	cases := []struct {
		name    string
		line    string
		wantHit bool
	}{
		{
			name:    "字面路径裸灌（容器客户端）",
			line:    "docker exec -i mysql mysql -uroot -prootpass < examples/mall/scripts/schema.sql",
			wantHit: true,
		},
		{
			name:    "变量引用裸灌（本次出事的写法）",
			line:    `  mysql_exec <"$SCHEMA_SQL" >/dev/null`,
			wantHit: true,
		},
		{
			name:    "宿主客户端裸灌",
			line:    "mysql -h127.0.0.1 -P3306 -uroot -p < examples/mall/scripts/schema.sql",
			wantHit: true,
		},
		{
			name:    "正解：走统一入口渲染",
			line:    "bash examples/scripts/schema.sh apply",
			wantHit: false,
		},
		{
			name:    "渲染后的临时文件（已替换过占位）",
			line:    `  mysql_root <"$rendered" >/dev/null`,
			wantHit: false,
		},
		{
			name:    "注释里的反面例子不该被当成命令",
			line:    "# 别写成 mysql -uroot -p < examples/mall/scripts/schema.sql（这是错的）",
			wantHit: false,
		},
		{
			name:    "md 标题里提到文件名",
			line:    "# 2. 跑起来：examples/mall/scripts/schema.sql",
			wantHit: false,
		},
	}

	for _, c := range cases {
		got := len(mysqlRawFeedLines(c.line, sqlFile)) > 0
		if got != c.wantHit {
			t.Errorf("%s：wantHit=%v got=%v（行：%s）", c.name, c.wantHit, got, c.line)
		}
	}
}
