package ipfs

import (
	"testing"
)

// kuboVectors are root CIDs that Kubo v0.43.1 printed for
//
//	ipfs add --only-hash --cid-version=1 --raw-leaves=true --chunker=size-<chunk> [--max-file-links=<links>]
//
// over newTestReader(size). TestKuboCompat re-derives them from a real Kubo
// binary when MORCORE_KUBO_BIN is set; links 0 means Kubo's default (174).
var kuboVectors = []struct {
	size  int64
	chunk int
	links int
	cid   string
}{
	// Empty file: one empty raw leaf.
	{0, 262144, 0, "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"},
	// Up to one chunk: the raw leaf is the root.
	{1, 262144, 0, "bafkreibcvwxqlcrmwzulcxfuyhzq47ghec56hdaumvcbnhntl67wga4jyq"},
	{262143, 262144, 0, "bafkreibwhxvudhpokz6bgk74iiwoimzurrvlwhegsmjb5z3szou7ver7im"},
	{262144, 262144, 0, "bafkreidemtpffvxst7k3g26s3az3rhzz6jbsqfctdys3e3hyedvd2kzdcy"},
	// Depth 1.
	{262145, 262144, 0, "bafybeihuvmmuxpnginsztsbeifrs6xb4mjkosxmyvrd3ud6h33evi7iqx4"},
	{10 << 20, 262144, 0, "bafybeihs7jt75cgrogopawrhw5sg3mc3cklwg5fuip2hyoqwvaljwjkx7m"},
	{174 * 262144, 262144, 0, "bafybeie6ljkwmvsffq5e3ottyzwzkloha5qwqu3e6wi6scw53esjy423dy"},
	// Depth 2, starting at 175 leaves where the second subtree holds one leaf.
	{174*262144 + 1, 262144, 0, "bafybeigspejv3cz7pe6wczxb6ivnmjtxynuc67j4a2d4hy2hyajkhahc7a"},
	{50<<20 + 4321, 262144, 0, "bafybeigeya4at6myqpri4vtkquyuqtfrn4ljdhnwdk7amvgdqgm7qcm5by"},
	{2 * 174 * 1024, 1024, 0, "bafybeidyablwj5mcofbedojfktupfnmofru267fyykeekvxu6fdq6ekpyu"},
	{174 * 174 * 1024, 1024, 0, "bafybeidvchdl3mpsfvmk7r3uc3szfttaz7u3ifai7vriuq3zjdvgkku66a"},
	// Depth 3.
	{174*174*1024 + 1, 1024, 0, "bafybeidvnei273lxpohsiqjf3hamhy7fjltihojkurnkqqhtx3yu3gcd2y"},
	{174*174*1024 + 3*174*1024 + 777, 1024, 0, "bafybeie2r6pelifdgjkwcn3paekl26olcdsofk3b4djgxhmmtlowpvnzfe"},
	// Other fan-outs: 1024 (as in Kubo's unixfs-v1-2025 profile), and narrow
	// trees of depth 5 and 7.
	{1025*1024 + 1, 1024, 1024, "bafybeibachcaff7a56t37vzbjp37dkm4rl4ewsnxgv7e5sj32vs5ji72ey"},
	{81*256 + 1, 256, 3, "bafybeiewhayi2c6ge534vqlkezhm7pt5bixauh37ifrafwjmqq76gxgvwu"},
	{64*100 + 50, 100, 2, "bafybeihy3ubcijxuvqlhazxmxxsqpompvtdu3g2e2h2a3al5arvyclj57e"},
}

func TestKuboVectors(t *testing.T) {
	for _, v := range kuboVectors {
		if testing.Short() && v.size > 8<<20 {
			continue
		}
		im, err := NewImporter(Options{ChunkSize: v.chunk, MaxLinks: v.links}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := im.ReadFrom(newTestReader(v.size)); err != nil {
			t.Fatal(err)
		}
		root, size, err := im.Finish()
		if err != nil {
			t.Fatal(err)
		}
		if size != uint64(v.size) {
			t.Errorf("size %d: importer reports %d bytes", v.size, size)
		}
		if root.String() != v.cid {
			t.Errorf("size %d chunk %d links %d: got %s, Kubo says %s", v.size, v.chunk, v.links, root, v.cid)
		}
	}
}
