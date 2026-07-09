# Mock of the Linode API v4, for tools/e2e-provision.sh only. Stdlib python, no
# pip, never talks to a real cloud. See tools/provision-mock/mock.py.
FROM python:3-alpine
WORKDIR /app
COPY mock.py /app/mock.py
EXPOSE 8080
CMD ["python3", "/app/mock.py"]
