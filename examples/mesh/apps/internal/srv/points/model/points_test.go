package model

import "testing"

// 表名是硬契约：必须与 examples/scripts 的 DDL 真源一致。
// 用显式 TableName() 固定下来，避免 gorm 复数化规则变动让表名漂移。
func TestTableNames(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"PointAccount", PointAccount{}.TableName(), "points_account"},
		{"PointsLog", PointsLog{}.TableName(), "points_log"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s.TableName() = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
