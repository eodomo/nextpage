(export $(grep -v '^#' .env | xargs) && curl -u ${USER}:${PASS} ${OLLAMASERVER}/api/tags)
