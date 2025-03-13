.PHONY: default
default: build

ifeq (, $(shell which mkosi))
$(error "mkosi couldn't be found, please install it and try again")
endif

.PHONY: clean
clean:
	sudo -E rm -Rf .cache/ mkosi.output/
	sudo -E $(shell command -v mkosi) clean

.PHONY: incus-osd
incus-osd:
	(cd incus-osd && go build ./cmd/incus-osd)
	strip incus-osd/incus-osd

.PHONY: static-analysis
static-analysis:
	(cd incus-osd && golangci-lint run)

.PHONY: build
build: incus-osd
	-mkosi genkey
	mkdir -p mkosi.images/base/mkosi.extra/boot/EFI/
	openssl x509 -in mkosi.crt -out mkosi.images/base/mkosi.extra/boot/EFI/mkosi.der -outform DER
	mkdir -p mkosi.images/base/mkosi.extra/usr/local/bin/
	cp incus-osd/incus-osd mkosi.images/base/mkosi.extra/usr/local/bin/
	sudo rm -Rf mkosi.output/base* mkosi.output/debug* mkosi.output/incus*
	sudo -E $(shell command -v mkosi) --cache-dir .cache/ build
	sudo chown $(shell id -u):$(shell id -g) mkosi.output

.PHONY: test
test:
	incus delete -f test-incus-os || true
	incus image delete incus-os || true

	qemu-img convert -f raw -O qcow2 $(shell ls mkosi.output/IncusOS_*.raw | grep -v usr | grep -v esp | sort | tail -1) os-image.qcow2
	incus image import --alias incus-os test/metadata.tar.xz os-image.qcow2
	rm os-image.qcow2

	incus init --vm incus-os test-incus-os \
		-c security.secureboot=false \
		-c limits.cpu=4 \
		-c limits.memory=8GiB \
		-d root,size=50GiB
	incus config device add test-incus-os vtpm tpm
	incus start test-incus-os --console
	sleep 3
	incus console test-incus-os

.PHONY: test-applications
test-applications:
	$(eval RELEASE := $(shell ls mkosi.output/*.efi | sed -e "s/.*_//g" -e "s/.efi//g" | sort -n | tail -1))
	incus exec test-incus-os -- mkdir -p /root/updates
	echo ${RELEASE} | incus file push - test-incus-os/root/updates/RELEASE

	incus file push mkosi.output/debug.raw test-incus-os/root/updates/
	incus file push mkosi.output/incus.raw test-incus-os/root/updates/

	incus exec test-incus-os -- systemctl restart incus-osd

.PHONY: test-update
test-update:
	$(eval RELEASE := $(shell ls mkosi.output/*.efi | sed -e "s/.*_//g" -e "s/.efi//g" | sort -n | tail -1))
	incus exec test-incus-os -- mkdir -p /root/updates
	echo ${RELEASE} | incus file push - test-incus-os/root/updates/RELEASE

	incus file push mkosi.output/IncusOS_${RELEASE}.efi test-incus-os/root/updates/
	incus file push mkosi.output/IncusOS_${RELEASE}.usr* test-incus-os/root/updates/
	incus file push mkosi.output/debug.raw test-incus-os/root/updates/
	incus file push mkosi.output/incus.raw test-incus-os/root/updates/

	incus exec test-incus-os -- systemctl restart incus-osd
