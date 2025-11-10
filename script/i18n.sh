#!/bin/bash

mapfile -t LANG < <(ls pkg/i18n/translations)
TEMPLATE="pkg/i18n/template.pot"
PODIR="pkg/i18n/translations/%s/LC_MESSAGES"
GIT_ROOT=$(git rev-parse --show-toplevel)

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

err() {
	printf "${red}%s${plain}\n" "$*" >&2
}

success() {
	printf "${green}%s${plain}\n" "$*"
}

info() {
	printf "${yellow}%s${plain}\n" "$*"
}

generate() {
	case $1 in
	"template")
		generate_template
		;;
	"en")
		generate_en
		;;
	*)
		err "invalid argument"
		;;
	esac
}

generate_template() {
	mapfile -t src < <(find . -name "*.go" | sort)
	xgettext -C --add-comments=TRANSLATORS: -kErrorT -kT -kTf -kN:1,2 --from-code=UTF-8 -o $TEMPLATE "${src[@]}"
}

generate_en() {
	local po_file
	po_file=$(printf "$PODIR/nezha.po" "en_US")
	local mo_file
	mo_file=$(printf "$PODIR/nezha.mo" "en_US")
	msginit --input=$TEMPLATE --locale=en_US.UTF-8 --output-file="$po_file" --no-translator
	msgfmt "$po_file" -o "$mo_file"
}

compile() {
	if [[ $# != 0 && "$1" != "" ]]; then
		compile_single "$1"
	else
		compile_all
	fi
}

compile_single() {
	local param="$1"
	local found=0

	for lang in "${LANG[@]}"; do
		if [[ "$lang" == "$param" ]]; then
			found=1
			break
		fi
	done

	if [[ $found == 0 ]]; then
		err "the language does not exist."
		return
	fi

	local po_file
	po_file=$(printf "$PODIR/nezha.po" "$param")
	local mo_file
	mo_file=$(printf "$PODIR/nezha.mo" "$param")

	msgfmt "$po_file" -o "$mo_file"
}

compile_all() {
	local po_file
	local mo_file
	for lang in "${LANG[@]}"; do
		po_file=$(printf "$PODIR/nezha.po" "$lang")
		mo_file=$(printf "$PODIR/nezha.mo" "$lang")

		msgfmt "$po_file" -o "$mo_file"
	done
}

update() {
	if [[ $# != 0 && "$1" != "" ]]; then
		update_single "$1"
	else
		update_all
	fi
}

update_single() {
	local param="$1"
	local found=0

	for lang in "${LANG[@]}"; do
		if [[ "$lang" == "$param" ]]; then
			found=1
			break
		fi
	done

	if [[ $found == 0 ]]; then
		err "the language does not exist."
		return
	fi

	local po_file
	po_file=$(printf "$PODIR/nezha.po" "$param")
	msgmerge -U "$po_file" $TEMPLATE
}

update_all() {
	for lang in "${LANG[@]}"; do
		local po_file
		po_file=$(printf "$PODIR/nezha.po" "$lang")
		msgmerge -U "$po_file" $TEMPLATE
	done
}

show_help() {
	echo "Usage: $0 [command] args"
	echo ""
	echo "Available commands:"
	echo "  update      Update .po from .pot"
	echo "  compile     Compile .mo from .po"
	echo "  generate    Generate template or en_US locale"
	echo ""
	echo "Examples:"
	echo "  $0 update            # Update all locales"
	echo "  $0 update zh_CN      # Update zh_CN locale"
	echo "  $0 compile           # Compile all locales"
	echo "  $0 compile zh_CN     # Compile zh_CN locale"
	echo "  $0 generate template # Generate template"
	echo "  $0 generate en       # Generate en_US locale"
}

version() { echo "$@" | awk -F. '{ printf("%d%03d%03d%03d\n", $1,$2,$3,$4); }'; }

main() {
	if [[ $(version "$BASH_VERSION") < $(version "4.0") ]]; then
  		err "This version of bash does not support mapfile"
		exit 1
	fi

	if [[ $PWD != "$GIT_ROOT" ]]; then
		err "Must execute in the project root"
		exit 1
	fi

	case "$1" in
	"update")
		update "$2"
		;;
	"compile")
		compile "$2"
		;;
	"generate")
		generate "$2"
		;;
	*)
		echo "Error: Unknown command '$1'"
		show_help
		exit 1
		;;
	esac
}

main "$@"
