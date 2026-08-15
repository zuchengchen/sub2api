#!/usr/bin/env bash
set -Eeuo pipefail

readonly LLAMA_SERVER_SHA256="05fcaf46bb8b58e1e6cd80d3199ba619c98a1842bd1b59ac70fb6d18e7a26788"
readonly YUFENG_MODEL_SHA256="c9766937ea76717be85261fe5518d328d4209fecc291bfa24d18b521d897adde"
readonly LLAMA_BUILD="10430"
readonly LLAMA_COMMIT="4c1a0af40"

: "${LLAMA_CPP_DIR:?set LLAMA_CPP_DIR to the extracted llama.cpp b10430 directory}"
: "${YUFENG_MODEL_PATH:?set YUFENG_MODEL_PATH to the Q4_K_M GGUF file}"

readonly bind_address="${YUFENG_BIND_ADDRESS:-127.0.0.1}"
readonly port="${YUFENG_PORT:-8088}"
readonly server_path="${LLAMA_CPP_DIR%/}/llama-server"

case "${bind_address}" in
  127.0.0.1 | ::1 | localhost) ;;
  *)
    if [[ "${YUFENG_ALLOW_NON_LOOPBACK:-false}" != "true" ]]; then
      echo "refusing non-loopback bind address ${bind_address}; set YUFENG_ALLOW_NON_LOOPBACK=true only on a controlled network" >&2
      exit 64
    fi
    ;;
esac

if [[ ! "${port}" =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then
  echo "YUFENG_PORT must be an integer from 1 through 65535" >&2
  exit 64
fi
if [[ ! -x "${server_path}" ]]; then
  echo "llama-server is not executable: ${server_path}" >&2
  exit 66
fi
if [[ ! -r "${YUFENG_MODEL_PATH}" ]]; then
  echo "YuFeng model is not readable: ${YUFENG_MODEL_PATH}" >&2
  exit 66
fi

if [[ -n "${OPENMP_LIB_DIR:-}" ]]; then
  if [[ ! -d "${OPENMP_LIB_DIR}" ]]; then
    echo "OPENMP_LIB_DIR is not a directory: ${OPENMP_LIB_DIR}" >&2
    exit 66
  fi
  export LD_LIBRARY_PATH="${OPENMP_LIB_DIR}:${LLAMA_CPP_DIR}${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"
else
  export LD_LIBRARY_PATH="${LLAMA_CPP_DIR}${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}"
fi

verify_sha256() {
  local expected="$1"
  local path="$2"
  local actual

  actual="$(sha256sum --binary "${path}" | awk '{print $1}')"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "SHA-256 mismatch for ${path}: expected ${expected}, got ${actual}" >&2
    exit 65
  fi
}

verify_sha256 "${LLAMA_SERVER_SHA256}" "${server_path}"
verify_sha256 "${YUFENG_MODEL_SHA256}" "${YUFENG_MODEL_PATH}"

version_output="$("${server_path}" --version 2>&1)"
if [[ "${version_output}" != *"build ${LLAMA_BUILD}"* ]] || [[ "${version_output}" != *"commit ${LLAMA_COMMIT}"* ]]; then
  echo "unexpected llama-server version: ${version_output}" >&2
  exit 65
fi

exec "${server_path}" \
  --model "${YUFENG_MODEL_PATH}" \
  --alias yufeng-xguard-q4 \
  --host "${bind_address}" \
  --port "${port}" \
  --ctx-size 4096 \
  --parallel 1 \
  --batch-size 512 \
  --ubatch-size 512 \
  --threads 16 \
  --threads-batch 16 \
  --flash-attn on \
  --load-mode none \
  --cache-ram 0 \
  --no-cache-prompt \
  --no-context-shift \
  --no-mmproj \
  --offline \
  --no-webui \
  --no-slots \
  --metrics \
  --n-gpu-layers 0 \
  --cors-origins localhost \
  --no-cors-credentials \
  --jinja \
  --log-verbosity 2
