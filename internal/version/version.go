// Package version 是版本号的唯一来源。
//
// 之前 api 与 gateway 各写一份常量，改一处漏一处；面板显示的版本号和
// 出站 User-Agent 里的版本号必须始终一致，否则查问题时对不上。
package version

// Version 由发布流程同步（见 .github/workflows/docker.yml 的校验步骤）。
const Version = "3.5.8"
