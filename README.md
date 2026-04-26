```shell
make pg.up
make pg.export
```

```shell
time make recreate pg.import-last

# ...
+------+-------+------+------+------+-----------+
| NAME | STATE | IPV4 | IPV6 | TYPE | SNAPSHOTS |
+------+-------+------+------+------+-----------+
incus info --resources | head -n5
System:
  UUID: 4ff85147-1f53-9349-ae7c-adad38143614
  Vendor: Apple Inc.
  Product: Apple Virtualization Generic Platform
  Version: 1
scripts/pg-dev-local import-last
==> Importing from /Users/a/p/pansen/colima-incus/var/pg-dev-2026-04-26T22:41:05.tar.gz...
Imported and started pg-dev

real    0m27.368s
user    0m1.674s
sys     0m2.401s
```
