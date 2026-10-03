(export $(grep -v '^#' .env | xargs) && curl -u ${OLLAMAUSER}:${OLLAMAPASS} ${OLLAMASERVER}/api/tags)
