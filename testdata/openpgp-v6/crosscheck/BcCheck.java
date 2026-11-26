// Cross-checks the fixtures one directory up with Bouncy Castle 1.87
// (bcprov, bcpg, bcutil). Run from that directory:
//   java -cp 'bcprov-jdk18on-1.87.jar:bcpg-jdk18on-1.87.jar:bcutil-jdk18on-1.87.jar' crosscheck/BcCheck.java
// It decrypts each message with its key, verifies the detached v6 signature
// (and refuses it over altered text) and decrypts the v4 incoming golden.
import java.io.ByteArrayInputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;

import org.bouncycastle.openpgp.api.OpenPGPApi;
import org.bouncycastle.openpgp.api.OpenPGPCertificate;
import org.bouncycastle.openpgp.api.OpenPGPKey;
import org.bouncycastle.openpgp.api.OpenPGPMessageInputStream;
import org.bouncycastle.openpgp.api.OpenPGPSignature;
import org.bouncycastle.openpgp.api.bc.BcOpenPGPApi;

public class BcCheck {
    static final OpenPGPApi API = new BcOpenPGPApi();

    static String read(String name) throws Exception {
        return Files.readString(Path.of(name), StandardCharsets.UTF_8);
    }

    static String decrypt(String key, String message) throws Exception {
        OpenPGPKey k = API.readKeyOrCertificate().parseKey(read(key));
        OpenPGPMessageInputStream in = API.decryptAndOrVerifyMessage()
                .addDecryptionKey(k)
                .process(new ByteArrayInputStream(message.getBytes(StandardCharsets.UTF_8)));
        byte[] plain = in.readAllBytes();
        in.close();
        return new String(plain, StandardCharsets.UTF_8);
    }

    static boolean verify(String text) throws Exception {
        OpenPGPCertificate cert = API.readKeyOrCertificate().parseCertificate(read("v6-pqc.pub.asc"));
        List<OpenPGPSignature.OpenPGPDocumentSignature> sigs = API.verifyDetachedSignature()
                .addSignatures(new ByteArrayInputStream(read("v6-pqc.sig.asc").getBytes(StandardCharsets.UTF_8)))
                .addVerificationCertificate(cert)
                .process(new ByteArrayInputStream(text.getBytes(StandardCharsets.UTF_8)));
        return sigs.size() == 1 && sigs.get(0).isValid();
    }

    public static void main(String[] args) throws Exception {
        String text = read("message.txt");
        int failures = 0;
        for (String k : new String[] {"v4", "v4-pqc", "v6-pqc"}) {
            String got = decrypt(k + ".sec.asc", read(k + ".msg.asc"));
            boolean ok = got.equals(text);
            failures += ok ? 0 : 1;
            System.out.println((ok ? "ok   " : "FAIL ") + k + ".msg.asc decrypts");
        }
        for (String k : new String[] {"v4", "v6", "v6-pqc", "v4-pqc"}) {
            OpenPGPCertificate c = API.readKeyOrCertificate().parseCertificate(read(k + ".pub.asc"));
            System.out.println("ok   " + k + ".pub.asc v" + c.getPrimaryKey().getVersion() + " algorithm "
                    + c.getPrimaryKey().getAlgorithm() + ", " + c.getKeys().size() + " keys");
        }
        boolean sig = verify(text), altered = verify(text + "x");
        failures += sig && !altered ? 0 : 1;
        System.out.println((sig && !altered ? "ok   " : "FAIL ") + "v6-pqc.sig.asc verifies, altered text refused");
        String eml = read("golden-incoming.v4.eml");
        String armored = eml.substring(eml.indexOf("-----BEGIN PGP MESSAGE-----"),
                eml.indexOf("-----END PGP MESSAGE-----") + "-----END PGP MESSAGE-----".length());
        boolean golden = decrypt("v4.sec.asc", armored).contains("Golden body.");
        failures += golden ? 0 : 1;
        System.out.println((golden ? "ok   " : "FAIL ") + "golden-incoming.v4.eml decrypts");
        System.exit(failures == 0 ? 0 : 1);
    }
}
