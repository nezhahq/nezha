#!/bin/sh

#========================================================
# master 分支安装脚本重定向
#========================================================

# 判断是否应使用中国镜像

geo_check() {
    api_list="https://blog.cloudflare.com/cdn-cgi/trace https://developers.cloudflare.com/cdn-cgi/trace"
    ua="Mozilla/5.0 (X11; Linux x86_64; rv:60.0) Gecko/20100101 Firefox/81.0"
    set -- "$api_list"
    for url in $api_list; do
        text="$(curl -A "$ua" -m 10 -s "$url")"
        endpoint="$(echo "$text" | sed -n 's/.*h=\([^ ]*\).*/\1/p')"
        if echo "$text" | grep -qw 'CN'; then
            isCN=true
            break
        elif echo "$url" | grep -q "$endpoint"; then
            break
        fi
    done
}

# 向用户确认是否使用中国镜像
geo_check

if [ "$isCN" = true ]; then
    read -p "检测到您的IP可能来自中国大陆，是否使用中国镜像? [y/n] " choice
    case "$choice" in
        y|Y)
            echo "将使用中国镜像..."
            USE_CN_MIRROR=true
            ;;
        n|N)
            echo "将使用国际镜像..."
            USE_CN_MIRROR=false
            ;;
        *)
            echo "输入无效,将使用国际镜像..."
            USE_CN_MIRROR=false
            ;;
    esac
else
    USE_CN_MIRROR=false
fi

# 默认引导用户使用 master 分支 v1 新面板安装脚本，但若使用了 install_agent 参数，则默认重定向至 v0
if echo "$@" | grep -q "install_agent"; then
    echo "检测到 v0 面板 install_agent 参数，将使用 v0 分支脚本..."
    echo "警告: v0 面板已停止维护，请尽快升级至 v1 面板，详见文档：https://nezha.wiki/，5s 后继续运行脚本"
    # 强制等待 5 秒
    sleep 5
    is_v1=false
else
    # 让用户选择是否执行新脚本
    echo "v1 面板已正式发布，v0 已停止维护，若您已安装 v0 面板，请尽快升级至 v1 面板"
    echo "v1 与 v0 有较大差异，详见文档：https://nezha.wiki/"
    echo "若您暂时不想升级，请输入 n 并按回车键以继续使用 v0 面板脚本"
    read -p "是否执行 v1 面板安装脚本? [y/n] " choice
    case "$choice" in
        n|N)
            is_v1=false
            ;;
        *)
            is_v1=true
            ;;
    esac
fi

if [ "$is_v1" = true ]; then
    echo "将使用 v1 面板安装脚本..."
    if [ "$USE_CN_MIRROR" = true ]; then
        shell_url="https://gitee.com/naibahq/scripts/raw/main/install.sh"
    else
        shell_url="https://raw.githubusercontent.com/nezhahq/scripts/main/install.sh"
    fi
    file_name="nezha.sh"
else
    echo "将使用 v0 面板安装脚本，脚本将会下载为nezha_v0.sh"
    if [ "$USE_CN_MIRROR" = true ]; then
        shell_url="https://gitee.com/naibahq/scripts/raw/v0/install.sh"
    else
        shell_url="https://raw.githubusercontent.com/nezhahq/scripts/refs/heads/v0/install.sh"
    fi
    file_name="nezha_v0.sh"
fi


if command -v wget >/dev/null 2>&1; then
    wget -O "/tmp/nezha.sh" "$shell_url"
elif command -v curl >/dev/null 2>&1; then
    curl -o "/tmp/nezha.sh" "$shell_url"
else
    echo "错误: 未找到 wget 或 curl，请安装其中任意一个后再试"
    exit 1
fi

chmod +x "/tmp/nezha.sh"
mv "/tmp/nezha.sh" "./$file_name"
# 携带原参数运行新脚本
exec ./"$file_name" "$@"